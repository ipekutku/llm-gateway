# Querying usage and estimated cost

v0.5 exposes usage through SQL, not an admin HTTP API. Connect to the gateway's migrated PostgreSQL database with a SQL client. Use a database role with `SELECT` permission on `usage_records` for reporting; gateway client API keys do not grant database access.

For the disposable local database from `make db`, open `psql` with these connection settings (enter its development password, `gateway`, when prompted):

```bash
psql -X -h 127.0.0.1 -p 55432 -U gateway -d gateway
```

This requires the PostgreSQL command-line client. Adjust the host, port, user, and database for your own instance; avoid putting passwords on the command line. The examples below use `psql` variables; other SQL clients should bind equivalent parameters rather than concatenate user input. Database connection settings can contain credentials: do not commit them or paste them into reports.

## Interpreting records

Each persisted row describes a request that passed body validation, including unknown models, upstream failures, timeouts, and cancellations. Authentication, gateway rate-limit, body-read, and validation rejections are excluded. Records contain metadata, not prompt or completion content. See the [schema](architecture.md#schema) for column definitions.

- `requested_model` is the client's model; `model` is the model reported by the answering provider, which may differ after fallback or be a dated snapshot. On failure, `model` is NULL. `provider` is NULL when the final outcome identifies no upstream, such as an unknown model or open circuit.
- `input_tokens` includes cache reads and writes. Those cache columns are subsets, not extra tokens. Total tokens are input plus output. All four token columns are NULL if usage is unknown.
- `cost_usd` is an estimate calculated at request time from the answering provider's configured model price. It is stored as an exact decimal in USD, not picodollars. Changing prices does not recalculate historical rows. NULL means unknown, while zero means a known zero estimate.
- `status` describes the client-facing outcome. `499` with `client_closed` is an internal accounting convention, never a response sent to a client. A canceled request can still have known usage and cost if the provider returned before the disconnect was observed.

Recording is best-effort and asynchronous. A partial batch normally flushes after one second, so a just-finished request may not appear immediately. Queue overflow, failed database writes, process crashes, and shutdown deadlines can lose records. Failed attempts before retries or fallback may incur unreported charges. Reports therefore describe persisted, provider-reported usage, not all traffic or an exact provider bill.

PostgreSQL aggregates skip NULL values. The queries retain NULL sums for groups with no known values and show coverage counts alongside known subtotals. Do not replace unknown costs with zero or describe a subtotal as a complete total when `records_without_cost` is nonzero. No rows also does not prove there were no requests.

## Reporting window

Set an inclusive start and exclusive end, with explicit UTC offsets. Replace these example dates with the period to report:

```sql
\set from '2026-10-01T00:00:00Z'
\set to '2026-11-01T00:00:00Z'
```

The half-open window avoids counting a boundary request twice in adjacent reports. Filters compare `received_at` directly, allowing the time-range indexes to be used; grouping by UTC day does not depend on the session's time zone. Day attribution uses request arrival, not completion or insertion time.

## Usage and cost per client

```sql
SELECT client_id,
       COUNT(*) AS recorded_requests,
       COUNT(*) FILTER (WHERE status BETWEEN 200 AND 299) AS successful_requests,
       COUNT(input_tokens) AS records_with_usage,
       COUNT(*) - COUNT(input_tokens) AS records_without_usage,
       SUM(input_tokens) AS known_input_tokens,
       SUM(cache_read_input_tokens) AS known_cache_read_input_tokens,
       SUM(cache_write_input_tokens) AS known_cache_write_input_tokens,
       SUM(output_tokens) AS known_output_tokens,
       SUM(input_tokens::bigint + output_tokens) AS known_total_tokens,
       COUNT(cost_usd) AS records_with_cost,
       COUNT(*) - COUNT(cost_usd) AS records_without_cost,
       SUM(cost_usd) AS known_cost_usd
FROM usage_records
WHERE received_at >= :'from'::timestamptz
  AND received_at < :'to'::timestamptz
GROUP BY client_id
ORDER BY client_id;
```

To report just one client, set `\set client 'my-app'` and add `AND client_id = :'client'` before `GROUP BY`. The `(client_id, received_at)` index supports that filter. Records remain queryable even if the client has been removed from the clients file.

## Usage and cost per answering model

```sql
SELECT provider, model,
       COUNT(*) AS recorded_requests,
       COUNT(input_tokens) AS records_with_usage,
       COUNT(*) - COUNT(input_tokens) AS records_without_usage,
       SUM(input_tokens) AS known_input_tokens,
       SUM(output_tokens) AS known_output_tokens,
       SUM(input_tokens::bigint + output_tokens) AS known_total_tokens,
       COUNT(cost_usd) AS records_with_cost,
       COUNT(*) - COUNT(cost_usd) AS records_without_cost,
       SUM(cost_usd) AS known_cost_usd
FROM usage_records
WHERE received_at >= :'from'::timestamptz
  AND received_at < :'to'::timestamptz
GROUP BY provider, model
ORDER BY provider NULLS LAST, model NULLS LAST;
```

This groups successful upstream usage by the actual responding provider and reported model, including fallback. NULL model groups retain failures without pretending that the requested model answered. To group demand instead, replace `provider, model` in the select, grouping, and ordering with `requested_model`; that includes failures and attributes fallback to the original request.

## Usage and cost per UTC day

```sql
SELECT (received_at AT TIME ZONE 'UTC')::date AS day_utc,
       COUNT(*) AS recorded_requests,
       COUNT(input_tokens) AS records_with_usage,
       COUNT(*) - COUNT(input_tokens) AS records_without_usage,
       SUM(input_tokens) AS known_input_tokens,
       SUM(output_tokens) AS known_output_tokens,
       SUM(input_tokens::bigint + output_tokens) AS known_total_tokens,
       COUNT(cost_usd) AS records_with_cost,
       COUNT(*) - COUNT(cost_usd) AS records_without_cost,
       SUM(cost_usd) AS known_cost_usd
FROM usage_records
WHERE received_at >= :'from'::timestamptz
  AND received_at < :'to'::timestamptz
GROUP BY day_utc
ORDER BY day_utc;
```

Only days with persisted rows appear. Apply the single-client filter above for a daily client report. Add `client_id`, or `provider, model`, to both the select and grouping for a daily breakdown.

## Inspecting a request

Use the value of the response's `X-Request-ID` header, not the successful body's `chatcmpl-` prefix:

```sql
\set request_id '<X-Request-ID value>'
SELECT request_id, received_at, duration_ms, client_id,
       requested_model, provider, model, status, error_code,
       input_tokens, cache_read_input_tokens, cache_write_input_tokens,
       output_tokens, cost_usd
FROM usage_records
WHERE request_id = :'request_id';
```

An absent row may mean the request was rejected before validation, its record has not flushed yet, or recording lost it. Failure logs carry the same request ID. The query uses the primary key and needs no reporting window.

## Retention

The gateway never deletes usage records, so the table grows with traffic. Choose how long to keep records and delete older ones periodically, for example from a scheduled job. Deleting needs a role with `DELETE` permission on `usage_records`; keep it separate from the read-only reporting role.

Delete in batches, so each statement is short and holds few locks while the gateway keeps inserting:

```sql
\set cutoff '2026-07-01T00:00:00Z'
DELETE FROM usage_records
WHERE request_id IN (
    SELECT request_id FROM usage_records
    WHERE received_at < :'cutoff'::timestamptz
    LIMIT 10000
);
```

Repeat until it reports `DELETE 0`. The `received_at` index finds the old rows, and PostgreSQL's autovacuum reclaims their space for new records. Reports for a window that starts before the cutoff will then be incomplete.

-- One row per validated request: its client, models,
-- outcome, token usage, and estimated cost. No prompt or completion content.
CREATE TABLE usage_records (
    request_id               text PRIMARY KEY,
    received_at              timestamptz NOT NULL,
    duration_ms              integer NOT NULL CHECK (duration_ms >= 0),
    client_id                text NOT NULL,
    requested_model          text NOT NULL,
    -- The provider that answered, or the last one that failed; NULL if no
    -- provider was called.
    provider                 text,
    -- The model the provider reported; NULL on failure.
    model                    text,
    -- The HTTP status sent to the client; 499 if the client disconnected.
    status                   smallint NOT NULL CHECK (status BETWEEN 100 AND 599),
    -- The public error code; NULL on success.
    error_code               text,
    -- Token usage as the provider reported it; all NULL if unknown. Cache
    -- reads and writes are part of input_tokens.
    input_tokens             integer CHECK (input_tokens >= 0),
    cache_read_input_tokens  integer CHECK (cache_read_input_tokens >= 0),
    cache_write_input_tokens integer CHECK (cache_write_input_tokens >= 0),
    output_tokens            integer CHECK (output_tokens >= 0),
    -- Estimated cost in US dollars; NULL if unknown, never 0 for unknown.
    cost_usd                 numeric(24, 12) CHECK (cost_usd >= 0),

    CHECK (
        (input_tokens IS NULL) = (cache_read_input_tokens IS NULL)
        AND (input_tokens IS NULL) = (cache_write_input_tokens IS NULL)
        AND (input_tokens IS NULL) = (output_tokens IS NULL)
    ),
    CHECK (cache_read_input_tokens + cache_write_input_tokens <= input_tokens),
    CHECK (cost_usd IS NULL OR input_tokens IS NOT NULL)
);

CREATE INDEX usage_records_received_at_idx ON usage_records (received_at);
CREATE INDEX usage_records_client_id_received_at_idx ON usage_records (client_id, received_at);

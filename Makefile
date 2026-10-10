# Developer and CI commands. CI runs `make check`'s targets as separate
# steps, so this file is the single list of verification commands.

GOVULNCHECK := golang.org/x/vuln/cmd/govulncheck@v1.8.0

# Local PostgreSQL for development and the integration tests. CI uses the
# same image as a service. The port avoids a PostgreSQL already on 5432.
DB_CONTAINER := llm-gateway-postgres
DB_IMAGE := postgres:18
DB_PORT ?= 55432
DB_URL := postgres://gateway:gateway@127.0.0.1:$(DB_PORT)/gateway?sslmode=disable

# Local Redis for the integration tests, without persistence. CI uses the
# same image as a service. The port avoids a Redis already on 6379.
REDIS_CONTAINER := llm-gateway-redis
REDIS_IMAGE := redis:8
REDIS_PORT ?= 56379
REDIS_URL := redis://127.0.0.1:$(REDIS_PORT)/0

# Local Prometheus, Grafana, and Jaeger for a gateway running on the host,
# and the loopback ports they are published on.
OBSERVABILITY := docker compose -f deploy/observability/compose.yaml
export PROMETHEUS_PORT ?= 9090
export GRAFANA_PORT ?= 3000
export JAEGER_UI_PORT ?= 16686
export OTLP_HTTP_PORT ?= 4318

.PHONY: check fmt vet test build vuln smoke db db-stop redis redis-stop observability observability-stop

## check: run every CI check
check: fmt vet test build vuln

## fmt: fail if any Go file is not gofmt-formatted
fmt:
	@unformatted=$$(gofmt -l .); \
	if [ -n "$$unformatted" ]; then \
		printf 'Run gofmt on these files:\n%s\n' "$$unformatted"; \
		exit 1; \
	fi

## vet: go vet, including the smoke test so it keeps compiling
vet:
	go vet ./...
	go vet -tags smoke ./cmd/gateway

## test: unit and end-to-end tests against fake providers. The PostgreSQL
## integration tests run only when GATEWAY_TEST_DATABASE_URL is set (see db),
## and the Redis ones only when GATEWAY_TEST_REDIS_URL is set (see redis).
test:
	go test -race -timeout 2m ./...

## build: build the gateway without leaving a binary behind
build:
	go build -o /dev/null ./cmd/gateway

## vuln: known vulnerabilities reachable from the code (needs network)
vuln:
	go run $(GOVULNCHECK) ./...

## smoke: call the REAL provider APIs (costs money; never run in CI).
## Needs OPENAI_MODEL and OPENAI_API_KEY, ANTHROPIC_MODEL and
## ANTHROPIC_API_KEY, or both.
smoke:
	go test -tags smoke -count=1 -v -timeout 5m -run '^TestSmoke$$' ./cmd/gateway

## db: start a disposable local PostgreSQL in Docker; its data is deleted
## when it stops. Prints the variable that enables the integration tests.
db:
	docker run -d --rm --name $(DB_CONTAINER) \
		-e POSTGRES_USER=gateway -e POSTGRES_PASSWORD=gateway \
		-p 127.0.0.1:$(DB_PORT):5432 $(DB_IMAGE)
	@until docker exec $(DB_CONTAINER) pg_isready -h 127.0.0.1 -U gateway -q; do sleep 1; done
	@echo "export GATEWAY_TEST_DATABASE_URL='$(DB_URL)'"

## db-stop: stop the local PostgreSQL and delete its data
db-stop:
	docker stop $(DB_CONTAINER)

## redis: start a disposable local Redis in Docker, without persistence.
## Prints the variable that enables the Redis integration tests.
redis:
	docker run -d --rm --name $(REDIS_CONTAINER) \
		-p 127.0.0.1:$(REDIS_PORT):6379 $(REDIS_IMAGE) \
		redis-server --save '' --appendonly no
	@until docker exec $(REDIS_CONTAINER) redis-cli ping >/dev/null 2>&1; do sleep 1; done
	@echo "export GATEWAY_TEST_REDIS_URL='$(REDIS_URL)'"

## redis-stop: stop the local Redis and delete its data
redis-stop:
	docker stop $(REDIS_CONTAINER)

## observability: start Prometheus, Grafana, and Jaeger in Docker for a
## gateway running on the host (see the README). Not needed by CI.
observability:
	$(OBSERVABILITY) up -d --wait
	@echo "Grafana:    http://127.0.0.1:$(GRAFANA_PORT)"
	@echo "Prometheus: http://127.0.0.1:$(PROMETHEUS_PORT)"
	@echo "Jaeger:     http://127.0.0.1:$(JAEGER_UI_PORT)"
	@echo "export OTEL_EXPORTER_OTLP_ENDPOINT='http://127.0.0.1:$(OTLP_HTTP_PORT)'"

## observability-stop: stop the stack and delete its data
observability-stop:
	$(OBSERVABILITY) down -v

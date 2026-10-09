# Developer and CI commands. CI runs `make check`'s targets as separate
# steps, so this file is the single list of verification commands.

GOVULNCHECK := golang.org/x/vuln/cmd/govulncheck@v1.8.0

# Local PostgreSQL for development and the integration tests. CI uses the
# same image as a service. The port avoids a PostgreSQL already on 5432.
DB_CONTAINER := llm-gateway-postgres
DB_IMAGE := postgres:18
DB_PORT ?= 55432
DB_URL := postgres://gateway:gateway@127.0.0.1:$(DB_PORT)/gateway?sslmode=disable

.PHONY: check fmt vet test build vuln smoke db db-stop

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
## integration tests run only when GATEWAY_TEST_DATABASE_URL is set (see db).
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

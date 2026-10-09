# Developer and CI commands. CI runs `make check`'s targets as separate
# steps, so this file is the single list of verification commands.

GOVULNCHECK := golang.org/x/vuln/cmd/govulncheck@v1.8.0

.PHONY: check fmt vet test build vuln smoke

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

## test: unit and end-to-end tests against fake providers
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

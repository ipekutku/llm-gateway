#!/usr/bin/env bash
# HTTPS smoke checks through the real hostname and certificate. No TLS bypass,
# real providers, or metrics exposure. Credentials reach curl only via a file.
set -euo pipefail
umask 077
domain=${TF_VAR_domain_name:-llm-gateway.utkuipek.dev}
session=${AWS_SESSION_DIR:-deploy/aws/session}
[[ $domain =~ ^[a-z0-9.-]+$ && $domain != -* ]] || { echo "Invalid gateway domain_name" >&2; exit 1; }
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
fail() { echo "FAIL: $*" >&2; exit 1; }
[[ -s $session/demo-key ]] || fail "$session/demo-key is missing"
printf 'Authorization: Bearer %s\n' "$(<"$session/demo-key")" >"$work/header"
url=https://$domain

curl -fsS --connect-timeout 10 --max-time 30 "$url/readyz" >/dev/null || fail "HTTPS readiness; check DNS, ALB targets, and your allowed IP"
status=$(curl -sS --connect-timeout 10 --max-time 30 -o /dev/null -w '%{http_code}' "$url/v1/chat/completions" \
	-H 'Content-Type: application/json' -d '{}')
[[ $status == 401 ]] || fail "Unauthenticated completion returned $status, expected 401"
for model in demo-openai demo-anthropic; do
	curl -fsS --connect-timeout 10 --max-time 30 "$url/v1/chat/completions" \
		-H @"$work/header" -H 'Content-Type: application/json' \
		-d "{\"model\":\"$model\",\"messages\":[{\"role\":\"user\",\"content\":\"synthetic ingress check\"}]}" >"$work/body" || fail "$model HTTPS completion"
	jq -e '.choices[0].message.content == "synthetic completion"' "$work/body" >/dev/null || fail "Unexpected synthetic completion"
	echo "ok: $model completion over verified HTTPS"
done
for path in /metrics /debug/pprof/; do
	status=$(curl -sS --connect-timeout 10 --max-time 30 -o /dev/null -w '%{http_code}' "$url$path")
	[[ $status == 404 ]] || fail "$path returned $status over the API ingress, expected 404"
done
echo "ok: gateway authentication required; metrics and pprof absent from HTTPS ingress"

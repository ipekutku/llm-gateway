#!/usr/bin/env bash
# No dependency removal or binary rewriting: compare builds of the same source.
set -euo pipefail
umask 077
cd "$(dirname "$0")/../.."
mkdir -p deploy/loadtest/results
size_dir=$(mktemp -d "$PWD/deploy/loadtest/results/$(date -u +%Y%m%dT%H%M%SZ)-binary-size.XXXXXX")
go build -o "$size_dir/gateway" ./cmd/gateway
go build -ldflags='-s -w' -o "$size_dir/gateway-stripped" ./cmd/gateway
go list -deps -f '{{if .Module}}{{.ImportPath}} {{.Module.Path}}{{else}}{{.ImportPath}} stdlib{{end}}' ./cmd/gateway > "$size_dir/packages.txt"
# A command's symbols use main.*, not its import path.
go list -f 'main {{.Module.Path}}' ./cmd/gateway >> "$size_dir/packages.txt"
go tool nm -size "$size_dir/gateway" > "$size_dir/symbols.txt"
{
    date -u
    git rev-parse HEAD
    git status --short --untracked-files=no
    go version
    go env GOOS GOARCH
    printf 'Unstripped bytes: %s\nStripped bytes: %s\n' "$(wc -c < "$size_dir/gateway")" "$(wc -c < "$size_dir/gateway-stripped")"
    printf 'Text-symbol bytes by owning module (not binary-size shares; excludes data/BSS/metadata):\n'
    awk -f deploy/loadtest/symbols.awk "$size_dir/packages.txt" "$size_dir/symbols.txt" | sort -nr
} > "$size_dir/summary.txt"
cat "$size_dir/summary.txt"
printf 'Matching binaries and raw symbols: %s\n' "$size_dir"

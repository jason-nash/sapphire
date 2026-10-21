#!/bin/bash
set -euo pipefail

project_root="$(cd "$(dirname "$0")/.." && pwd)"
cd "$project_root"
git diff --check
if [[ "$(uname -s)" == Darwin ]]; then
  mkdir -p benchmarks/build
  cc -O2 -Wall -Wextra -Werror benchmarks/footprint.c -o benchmarks/build/footprint
fi
python3 -m unittest discover -s benchmarks -v
(
  cd engine
  go vet ./...
  go test -race ./...
)
(
  cd guest
  go vet ./...
  go test ./...
  CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o /dev/null ./cmd/sapphire-guest
)
if [[ "$(uname -s)" == Darwin && "$(uname -m)" == arm64 ]]; then
  make -C native all test sanitize
  swift test
fi

#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
go test -run 'TestMock' -v ./...

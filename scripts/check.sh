#!/bin/sh
set -eu

cd "$(dirname "$0")/.."

go tool pd doctor .
go tool pd gen --check --ts .
go build ./...
go test ./...
npm run build --prefix ts

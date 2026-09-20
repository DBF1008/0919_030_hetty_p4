#!/bin/sh
# test.sh - Build, vet, and run all unit tests for hetty.
#
# Usage:
#   ./test.sh          Run build, vet, and all unit tests.
#   RACE=1 ./test.sh   Additionally enable the race detector for tests.
#
# Note: cmd/hetty requires the embedded admin frontend (cmd/hetty/admin).
# Run `make build-admin` first if it is missing.

set -e
cd "$(dirname "$0")"

echo "==> go build ./..."
go build ./...

echo "==> go vet ./..."
go vet ./...

TESTFLAGS="-count=1 -v"
if [ "$RACE" = "1" ]; then
	TESTFLAGS="$TESTFLAGS -race"
fi

echo "==> go test $TESTFLAGS ./..."
# shellcheck disable=SC2086
go test $TESTFLAGS ./...

echo "==> All checks passed."

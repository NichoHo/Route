#!/bin/sh
set -e

echo "=== Checking gofmt ==="
UNFORMATTED=$(gofmt -l .)
if [ -n "$UNFORMATTED" ]; then
	echo "The following files are not gofmt'd:"
	echo "$UNFORMATTED"
	exit 1
fi

echo "=== Running go vet ==="
go vet ./...

echo "=== Running go mod verify ==="
go mod verify

echo "=== Building ==="
CGO_ENABLED=1 go build ./...

echo "=== Running tests ==="
CGO_ENABLED=1 go test -p 1 ./...

echo "=== ALL DONE ==="

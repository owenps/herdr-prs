#!/bin/sh
set -eu
cd "$(dirname "$0")/.."
mkdir -p bin
go build -trimpath -o bin/herdr-prs ./cmd/herdr-prs

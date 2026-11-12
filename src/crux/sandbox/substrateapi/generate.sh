#!/usr/bin/env sh
# Regenerates the *.pb.go files from the trimmed *.proto subset.
# Requires buf, protoc-gen-go (v1.36.x) and protoc-gen-go-grpc on PATH.
set -eu
cd "$(dirname "$0")"
buf generate

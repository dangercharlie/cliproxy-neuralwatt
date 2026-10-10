#!/bin/sh
# Build the plugin as a Linux shared library inside a Debian-based Go image so
# the result links against the same glibc generation as the CLIProxyAPI image.
# Usage: scripts/build-linux.sh [amd64|arm64]   -> dist/linux/<arch>/neuralwatt.so
set -eu
ARCH="${1:-amd64}"
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
IMAGE="${GO_IMAGE:-golang:1.27-bookworm}"
DOCKER="${DOCKER:-docker}"
mkdir -p "$ROOT/dist/linux/$ARCH" "$ROOT/.gocache/mod" "$ROOT/.gocache/build"
$DOCKER run --rm \
  -v "$ROOT:/src" -w /src \
  -v "$ROOT/.gocache/mod:/go/pkg/mod" -v "$ROOT/.gocache/build:/root/.cache/go-build" \
  -e CGO_ENABLED=1 -e GOOS=linux -e GOARCH="$ARCH" -e GOFLAGS=-mod=mod -e GOTOOLCHAIN=local \
  "$IMAGE" sh -c 'go build -trimpath -buildmode=c-shared -ldflags="-s -w" -o "dist/linux/$GOARCH/neuralwatt.so" . && rm -f "dist/linux/$GOARCH/neuralwatt.h" && ls -la "dist/linux/$GOARCH/"'

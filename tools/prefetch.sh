#!/usr/bin/env bash
# Explicit, network-enabled dependency prefetch. The rest of the pipeline runs
# with --network none. Nothing fetched here is committed: node_modules and
# .cache are gitignored. Pinned versions come from package-lock.json,
# backend/go.mod and tools/go.mod; oapi-codegen is built at v2.8.0.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
NODE_IMAGE="${NODE_IMAGE:-node:22-bookworm}"
GO_IMAGE="${GO_IMAGE:-golang:1.26}"
UID_GID="$(id -u):$(id -g)"

mkdir -p "$ROOT/.cache/npm" "$ROOT/.cache/gopath" "$ROOT/.cache/gomod" \
  "$ROOT/.cache/gobuild" "$ROOT/.cache/bin"

echo "prefetch: npm ci ($NODE_IMAGE)"
docker run --rm \
  --user "$UID_GID" \
  -e HOME=/tmp/home \
  -e npm_config_cache=/work/.cache/npm \
  -e NO_UPDATE_NOTIFIER=1 \
  -v "$ROOT:/work" \
  -w /work \
  "$NODE_IMAGE" \
  bash -c 'mkdir -p /tmp/home && npm ci --ignore-scripts --no-audit --no-fund'

echo "prefetch: build pinned oapi-codegen v2.8.0 ($GO_IMAGE)"
docker run --rm \
  --user "$UID_GID" \
  -e HOME=/tmp/home \
  -e GOPATH=/work/.cache/gopath \
  -e GOMODCACHE=/work/.cache/gomod \
  -e GOCACHE=/work/.cache/gobuild \
  -v "$ROOT:/work" \
  -w /work/tools \
  "$GO_IMAGE" \
  bash -c 'mkdir -p /tmp/home && go mod download && go install github.com/oapi-codegen/oapi-codegen/v2/cmd/oapi-codegen@v2.8.0 && cp /work/.cache/gopath/bin/oapi-codegen /work/.cache/bin/oapi-codegen'

echo "prefetch: backend module cache for offline go test"
docker run --rm \
  --user "$UID_GID" \
  -e HOME=/tmp/home \
  -e GOPATH=/work/.cache/gopath \
  -e GOMODCACHE=/work/.cache/gomod \
  -e GOCACHE=/work/.cache/gobuild \
  -v "$ROOT:/work" \
  -w /work/backend \
  "$GO_IMAGE" \
  bash -c 'mkdir -p /tmp/home && go mod download'

echo "prefetch: done"

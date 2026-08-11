#!/usr/bin/env bash

# Structurally rewrite Kubernetes manifest image references to immutable
# digests. The Go helper resolves YAML flow/block syntax, anchors, aliases, and
# folded args without interpreting image-looking text inside ConfigMap data.

set -euo pipefail

if [ "$#" -eq 0 ]; then
    echo "usage: $0 IMAGE=IMAGE@sha256:DIGEST [...]" >&2
    exit 2
fi

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"
cd "$ROOT_DIR"
exec go run ./hack/pin-manifest-images.go "$@"

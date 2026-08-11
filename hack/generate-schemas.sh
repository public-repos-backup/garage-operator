#!/usr/bin/env bash
# shellcheck shell=bash

# Generate JSON schemas from CRDs for editor validation
# Works in CI pipelines - auto-installs PyYAML if needed

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
OUTPUT_DIR="${1:-${PROJECT_ROOT}/schemas}"
CRD_DIR="${2:-${PROJECT_ROOT}/config/crd/bases}"

# Ensure Python 3 is available
if ! command -v python3 &> /dev/null; then
    echo "Error: python3 is required but not installed."
    exit 1
fi

# Check if PyYAML is installed, install if not
if ! python3 -c "import yaml" 2>/dev/null; then
    echo "Installing PyYAML..."
    pip3 install --user 'PyYAML==6.0.3' 2>/dev/null || pip3 install 'PyYAML==6.0.3' --break-system-packages 2>/dev/null || {
        echo "Error: Failed to install PyYAML 6.0.3. Please install it manually: pip3 install PyYAML==6.0.3"
        exit 1
    }
fi

mkdir -p "${OUTPUT_DIR}"

# Generate into a fresh staging directory so schemas for deleted CRDs cannot
# linger indefinitely in the committed output.
STAGING_DIR="$(mktemp -d)"
trap 'rm -rf -- "${STAGING_DIR}"' EXIT
python3 "${SCRIPT_DIR}/openapi2jsonschema.py" "${STAGING_DIR}" "${CRD_DIR}"/*.yaml

for existing in "${OUTPUT_DIR}"/*.json; do
    [ -e "${existing}" ] || continue
    generated="${STAGING_DIR}/$(basename "${existing}")"
    if [ ! -f "${generated}" ]; then
        rm -f -- "${existing}"
    fi
done
cp "${STAGING_DIR}"/*.json "${OUTPUT_DIR}/"

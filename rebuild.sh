#!/bin/sh
# Build the GuriMoon server. Needs Go 1.24 or newer.
#
#   ./rebuild.sh              build ./gurimoon
#   ./rebuild.sh -o PATH      write the executable to PATH (the Dockerfile uses this)
set -eu

root=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
out="$root/gurimoon"

while [ $# -gt 0 ]; do
    case "$1" in
        -o) out="$2"; shift 2 ;;
        *) echo "Usage: ./rebuild.sh [-o PATH]"; exit 1 ;;
    esac
done

(cd "$root" && go build -o "$out" ./cmd/gurimoon/) || { echo "=== BUILD FAILED ==="; exit 1; }
echo "=== Build OK: $out ==="

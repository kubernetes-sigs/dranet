#!/bin/bash
set -euo pipefail

python -m pip install --no-cache-dir nixl

exec python /bench/nixl_benchmark.py "$@"

#!/usr/bin/env bash
set -euo pipefail
export LC_ALL=C

if [[ $# -ne 1 ]]; then
    echo "Usage: $0 <linux-amd64-binary>" >&2
    exit 2
fi

binary=$1
notes=$(readelf --notes "$binary")
printf '%s\n' "$notes"
if grep -Eq 'x86 ISA needed:.*x86-64-v[2-9]' <<< "$notes"; then
    echo "Release binary requires more than the x86-64 baseline." >&2
    exit 1
fi

# Exercise the loader and process initialization without opening chain data or
# network connections. qemu64 has neither AVX nor SSE4; Haswell has AVX2.
for cpu in qemu64 Haswell-noTSX; do
    echo "Checking startup with CPU model: $cpu"
    timeout 30s qemu-x86_64 -cpu "$cpu" "$binary" --version
done

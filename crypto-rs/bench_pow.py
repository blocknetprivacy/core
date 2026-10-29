#!/usr/bin/env python3
"""Linux PoW timing/page-fault probe using the daemon's existing C ABI.

Build with: cargo build --release --manifest-path crypto-rs/Cargo.toml
Run with: python3 crypto-rs/bench_pow.py PATH_TO_LIBRARY --label BUILD [--cpu CPU]
Use the same script, CPU and build flags for each library. Run each comparison
in a fresh process, alternating build order. Keep other test workloads separate.
"""
import argparse
import ctypes
import json
import os
from pathlib import Path
import platform
import resource
import time


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("library", type=Path)
    parser.add_argument("--label", required=True)
    parser.add_argument("--hashes", type=int, default=5)
    parser.add_argument("--cpu", type=int)
    args = parser.parse_args()
    if args.hashes < 2:
        parser.error("at least two hashes are required")
    if args.cpu is not None:
        os.sched_setaffinity(0, {args.cpu})

    library = ctypes.CDLL(str(args.library.resolve()))
    byte_pointer = ctypes.POINTER(ctypes.c_uint8)
    library.blocknet_pow_hash.argtypes = [
        byte_pointer, ctypes.c_size_t, ctypes.c_uint64, byte_pointer
    ]
    library.blocknet_pow_hash.restype = ctypes.c_int
    header = (ctypes.c_uint8 * 80)(*([0x5A] * 80))
    samples = []
    for nonce in range(args.hashes):
        output = (ctypes.c_uint8 * 32)()
        before = resource.getrusage(resource.RUSAGE_SELF)
        start = time.perf_counter_ns()
        result = library.blocknet_pow_hash(header, len(header), nonce, output)
        elapsed = (time.perf_counter_ns() - start) / 1e6
        after = resource.getrusage(resource.RUSAGE_SELF)
        if result != 0:
            raise RuntimeError(f"hash failed: {result}")
        samples.append({
            "nonce": nonce, "ms": elapsed,
            "minor_faults": after.ru_minflt - before.ru_minflt,
            "major_faults": after.ru_majflt - before.ru_majflt,
            "hash": bytes(output).hex(),
        })

    memory = {}
    for line in Path("/proc/self/smaps_rollup").read_text().splitlines():
        if ":" in line:
            key, value = line.split(":", 1)
            if key in ("Rss", "Anonymous", "AnonHugePages"):
                memory[key + "_kib"] = int(value.split()[0])
    print(json.dumps({
        "label": args.label, "cpu": args.cpu, "platform": platform.platform(),
        "samples": samples, "memory": memory,
        "maxrss_kib": resource.getrusage(resource.RUSAGE_SELF).ru_maxrss,
    }))


if __name__ == "__main__":
    main()

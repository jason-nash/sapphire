#!/usr/bin/env python3
"""Capture macOS process-tree resources and fail closed on unmatched comparisons."""

import argparse
import hashlib
import json
import math
import os
from pathlib import Path
import platform
import subprocess
import sys
import time


def percentile(values, fraction):
    return sorted(values)[min(len(values) - 1, math.ceil(len(values) * fraction) - 1)]


def summarize(samples):
    intervals = []
    previous = None
    identities = None
    stable = True
    for sample in samples:
        current = {(p["pid"], p["start"]): p for p in sample["processes"]}
        if identities is not None and current.keys() != identities:
            stable = False
        identities = current.keys()
        if previous is not None:
            elapsed = sample["elapsed"] - previous["elapsed"]
            earlier = {(p["pid"], p["start"]): p for p in previous["processes"]}
            if current.keys() == earlier.keys():
                delta = {
                    key: sum(p[key] - earlier[identity][key] for identity, p in current.items())
                    for key in ("cpuNs", "idleWakeups", "interruptWakeups")
                }
                if elapsed <= 0 or any(value < 0 for value in delta.values()):
                    raise ValueError("invalid cumulative resource counter")
                intervals.append({
                    "seconds": elapsed,
                    **delta,
                    "cpuPercent": delta["cpuNs"] / (elapsed * 1e9) * 100,
                    "idleWakeupsPerSecond": delta["idleWakeups"] / elapsed,
                    "interruptWakeupsPerSecond": delta["interruptWakeups"] / elapsed,
                })
        previous = sample
    footprints = [sum(p["footprint"] for p in sample["processes"]) for sample in samples]
    measured_seconds = sum(i["seconds"] for i in intervals)
    return {
        "stableProcessSet": stable,
        "durationSeconds": samples[-1]["elapsed"] - samples[0]["elapsed"],
        "footprintP95Bytes": percentile(footprints, .95),
        "footprintPeakBytes": max(footprints),
        "cpuMeanPercent": sum(i["cpuNs"] for i in intervals) / (measured_seconds * 1e9) * 100 if intervals else None,
        "cpuP95Percent": percentile([i["cpuPercent"] for i in intervals], .95) if intervals else None,
        "idleWakeupsPerSecond": sum(i["idleWakeups"] for i in intervals) / measured_seconds if intervals else None,
        "interruptWakeupsPerSecond": sum(i["interruptWakeups"] for i in intervals) / measured_seconds if intervals else None,
    }


def capture(args):
    if sys.platform != "darwin":
        raise ValueError("host footprint capture requires macOS")
    if args.seconds < 10 or args.interval < .1 or args.seconds < 2 * args.interval:
        raise ValueError("capture at least 10 seconds with interval >= 0.1 seconds and at least three samples")
    specification = Path(args.specification).read_bytes()
    config = json.loads(specification)
    required = ("workload", "cpus", "memoryMiB", "architecture", "images", "conditions")
    if any(key not in config for key in required) or config["workload"] != args.workload:
        raise ValueError("specification must identify workload, cpus, memoryMiB, architecture, images, and conditions")
    sampler = Path(__file__).resolve().parent / "build" / "footprint"
    if not sampler.is_file():
        raise ValueError("build the sampler: mkdir -p benchmarks/build && cc -O2 -Wall -Wextra -Werror benchmarks/footprint.c -o benchmarks/build/footprint")
    samples = []
    started = time.monotonic()
    deadline = None
    while True:
        output = subprocess.check_output([str(sampler), *map(str, args.pid)], text=True)
        samples.append({"elapsed": time.monotonic() - started, "processes": json.loads(output)})
        if deadline is None:
            deadline = started + samples[0]["elapsed"] + args.seconds
        now = time.monotonic()
        if now >= deadline:
            break
        time.sleep(min(args.interval, deadline - now))
    data = {
        "schemaVersion": 1,
        "backend": args.backend,
        "backendVersion": args.version,
        "workload": args.workload,
        "phase": args.phase,
        "isolation": args.isolation,
        "isolationEvidence": args.isolation_evidence,
        "host": {"platform": platform.platform(), "machine": platform.machine(), "cpus": os.cpu_count()},
        "specification": config,
        "specificationSHA256": hashlib.sha256(specification).hexdigest(),
        "roots": args.pid,
        "capturedAt": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()),
        "summary": summarize(samples),
        "samples": samples,
    }
    destination = Path(args.output)
    destination.parent.mkdir(parents=True, exist_ok=True)
    with destination.open("x") as output:
        json.dump(data, output, indent=2)
        output.write("\n")
    print(json.dumps(data["summary"], indent=2))
    if args.isolation != "exclusive":
        print("Shared-host observation; excluded from a parity pass.", file=sys.stderr)


METRICS = ("footprintP95Bytes", "footprintPeakBytes", "cpuMeanPercent", "cpuP95Percent",
           "idleWakeupsPerSecond", "interruptWakeupsPerSecond")
WORKLOADS = ("container", "machine", "k8s")
PHASES = ("idle", "loaded", "reclaimed")


def compare_records(records):
    indexed = {}
    errors = []
    rows = []
    for record in records:
        key = record["backend"], record["workload"], record["phase"]
        if key in indexed:
            errors.append(f"duplicate capture: {key}")
        indexed[key] = record
    for workload in WORKLOADS:
        for phase in PHASES:
            key = workload, phase
            candidate = indexed.get(("sapphire", *key))
            reference = indexed.get(("orbstack", *key))
            if candidate is None or reference is None:
                errors.append(f"{workload}/{phase}: missing matched captures")
                continue
            pair_valid = True
            for capture_record in (candidate, reference):
                if capture_record.get("isolation") != "exclusive" or not capture_record.get("isolationEvidence"):
                    errors.append(f"{capture_record['backend']}/{workload}/{phase}: requires documented exclusive baseline")
                    pair_valid = False
                summary = capture_record["summary"]
                if not summary["stableProcessSet"] or summary["durationSeconds"] < 60:
                    errors.append(f"{capture_record['backend']}/{workload}/{phase}: requires >=60s with stable process identities")
                    pair_valid = False
            if candidate["specificationSHA256"] != reference["specificationSHA256"] or candidate["host"] != reference["host"]:
                errors.append(f"{workload}/{phase}: host or workload specification differs")
                pair_valid = False
            if not pair_valid:
                continue
            for metric in METRICS:
                measured, baseline = candidate["summary"][metric], reference["summary"][metric]
                if measured is None or baseline is None or not math.isfinite(measured) or not math.isfinite(baseline):
                    errors.append(f"{workload}/{phase}/{metric}: missing or invalid measurement")
                    continue
                passed = measured <= baseline
                rows.append({"workload": workload, "phase": phase, "metric": metric,
                             "sapphire": measured, "orbstack": baseline, "passed": passed})
                if not passed:
                    errors.append(f"{workload}/{phase}/{metric}: Sapphire exceeds OrbStack")
    return {"passed": not errors, "scope": "host process-tree memory, CPU, and wakeups only",
            "errors": errors, "comparisons": rows}


def compare(args):
    result = compare_records([json.loads(Path(path).read_text()) for path in args.captures])
    print(json.dumps(result, indent=2))
    return 0 if result["passed"] else 1


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    commands = parser.add_subparsers(dest="command", required=True)
    measurement = commands.add_parser("capture")
    measurement.add_argument("--pid", type=int, action="append", required=True, help="root PID; descendants included")
    measurement.add_argument("--backend", choices=("sapphire", "orbstack"), required=True)
    measurement.add_argument("--version", required=True)
    measurement.add_argument("--workload", choices=WORKLOADS, required=True)
    measurement.add_argument("--phase", choices=PHASES, required=True)
    measurement.add_argument("--specification", required=True)
    measurement.add_argument("--isolation", choices=("shared", "exclusive"), default="shared")
    measurement.add_argument("--isolation-evidence", default="", help="describe verified absence of unrelated workloads")
    measurement.add_argument("--seconds", type=float, default=120)
    measurement.add_argument("--interval", type=float, default=1)
    measurement.add_argument("--output", required=True)
    comparison = commands.add_parser("compare")
    comparison.add_argument("captures", nargs="+")
    args = parser.parse_args()
    try:
        if args.command == "capture":
            capture(args)
            return 0
        return compare(args)
    except (OSError, ValueError, KeyError, subprocess.CalledProcessError) as error:
        parser.error(str(error))


if __name__ == "__main__":
    sys.exit(main())

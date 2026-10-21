# Resource acceptance

Sapphire's target is similar or lower resource consumption than OrbStack for
containers, shared-kernel Linux machines, and Kubernetes. Performance parity
has not been established.

## Required design properties

| Area | Requirement | Evidence needed |
| --- | --- | --- |
| Kernel | One hardware VM and one shared kernel | Host process tree; guest boot ID and machine namespace checks |
| Guest memory | Demand-backed capacity; free memory returned to host | macOS physical footprint before allocation, at peak, and after release |
| vCPU idle | Block waiting for interrupts/events | CPU-time deltas, package idle wakeups, interrupt wakeups |
| Devices | Bounded rings, batching, notification suppression where supported | Queue tests and I/O profiles under matched workloads |
| Networking | No second helper VM; event-driven forwarding | Process tree; throughput, latency, CPU, connection cleanup |
| Storage | Sparse allocation and explicit flush semantics | Allocated bytes, fsync/persistence tests, random/sequential I/O |
| Machines | Namespace/cgroup overhead rather than another kernel | Incremental host footprint per machine; actual CPU/memory limits |
| Kubernetes | Reuse guest kernel and engine; start on demand | Incremental footprint, ready latency, loaded throughput, stop reclamation |
| UI/control | No persistent background refresh loop | Idle process counters with UI open and closed |
| Shutdown | Release VM memory and owned listeners/processes | Post-stop process, socket, and memory checks |

No design shortcut may silently discard required durability, isolation, or
workload correctness to lower a benchmark number.

The implementation uses a blocking host event loop and native GIC delivery,
suppresses redundant interrupt assertions, and runs one host network stack.
Machines add namespace/cgroup state instead of a second kernel. Docker's
port watcher subscribes to events and takes a snapshot when those events
arrive. Service readiness retries are bounded to lifecycle operations.
These decisions reduce avoidable work; their actual costs still need
measurement. Synchronous block I/O, unaccelerated virtio networking, guest
page cache, and the Kubernetes control plane remain profiling targets.

## Matched workload matrix

For **each** backend, measure container, machine, and Kubernetes workloads in
idle, loaded, and reclaimed phases. Use at least 120 seconds per stable phase,
three independent runs, and the same physical host, CPU/memory limits, guest
architecture, image digests, workload commands, readiness conditions, data,
network path, cache state, and client load. Report medians and raw runs; retain
variance and outliers. The comparator's minimum window is 60 seconds so small
smoke captures cannot pass it.

Match the trust mode too. Sapphire's initial machine mode gives root
administrative access to the shared guest. Its ordinary cgroup/disk limits do
not establish isolation from that root user. A comparison with an isolated
machine mode cannot establish equal-cost isolation. Untrusted machine
isolation remains an unsupported feature, independently of resource results.

Suggested workload definitions:

- Container: one digest-pinned HTTP service, a fixed request rate/payload, and a
  separate memory grow/free process with a known working set.
- Machine: the same distribution/rootfs and service inside one namespace-based
  machine; repeat with 1, 5, and 10 idle machines.
- Kubernetes: one node and the same HTTP service replica count, pinned k3s
  version, fixed client load, and a memory grow/free pod.

Record cold boot, warm start, service-ready time, throughput, request latency,
filesystem latency, disk allocation, peak footprint, steady footprint, CPU,
wakeups, and post-workload reclamation. Startup and throughput require their
own timed functional workload harness; the process sampler does not measure
them.

An already-running OrbStack instance containing unrelated work is a
**shared-host observation**, not a valid parity baseline. Do not stop or
reconfigure another workload to obtain a baseline without its owner's
authorization. Use a dedicated test host or a separately authorized window.

## Host process sampler

Build:

```sh
mkdir -p benchmarks/build
cc -O2 -Wall -Wextra -Werror benchmarks/footprint.c -o benchmarks/build/footprint
python3 -m unittest discover -s benchmarks -v
```

Create a JSON specification shared byte-for-byte between matched runs:

```json
{
  "workload": "container",
  "cpus": 4,
  "memoryMiB": 4096,
  "architecture": "arm64",
  "images": ["registry.example/service@sha256:REPLACE_WITH_REAL_DIGEST"],
  "conditions": {
    "command": "REPLACE_WITH_EXACT_WORKLOAD_AND_LOAD_COMMAND",
    "cache": "warm",
    "rate": "REPLACE_WITH_FIXED_REQUEST_RATE",
    "phaseTransitions": "REPLACE_WITH_ALLOCATION_AND_RELEASE_CONDITIONS"
  }
}
```

Identify and verify the backend's root PID immediately before capturing. Pass
multiple `--pid` options if its processes have distinct parents. Include the UI
for a UI-to-UI comparison and exclude it for both backends for a headless
comparison.

```sh
python3 benchmarks/resources.py capture \
  --backend sapphire --version EXACT_COMMIT \
  --pid VERIFIED_ENGINE_PID \
  --workload container --phase idle --specification /tmp/container.json \
  --seconds 120 --output benchmarks/results/sapphire-container-idle.json
```

Capture defaults to `--isolation shared`. For an independently verified,
dedicated run, supply `--isolation exclusive --isolation-evidence '...'`.
This field records the operator's verification; the sampler cannot discover
all unrelated guest work or establish host exclusivity automatically.

Repeat the nine workload/phase combinations for each backend, then compare:

```sh
python3 benchmarks/resources.py compare benchmarks/results/*.json
```

The comparator requires a complete matrix, stable process identities, matched
specifications/host metadata, exclusive-baseline evidence, and non-regressing
memory, CPU, and wakeup measurements. It accepts one capture per matrix cell;
run it separately for each independent repetition. Its pass applies only to
the resource metrics it measures. Functional correctness, startup, throughput,
storage, reclamation timing, power, and broader product parity require the
additional checks described here.

## Integrated memory smoke observations

After building the independent assets and macOS sampler, run:

```sh
python3 scripts/smoke-runtime.py --with-k8s --resource-seconds 120 --keep
```

The harness uses a private runtime and explicit Docker endpoint. For each
workload it starts a sleeping service, captures idle resources, writes
256 MiB into a dedicated tmpfs mount, captures resources while the allocation
is retained, unlinks the file, and captures resources again. It waits ten
seconds after each transition before sampling. It does not drop caches.
Kubernetes uses a memory-backed `emptyDir`; machines use their own mount
namespace; containers use Docker's tmpfs support.

Each capture includes the engine and VMM process tree. The runtime and
functional record retain artifact hashes and exact phase specifications.
This is a sequential observation after functional checks, with warmed state
and unrelated host work present. It does not measure HTTP throughput or
energy, isolate the incremental cost of each service, or qualify as a clean
OrbStack baseline. `--resource-seconds 10` is useful for diagnosing the
capture path but remains below the comparator's minimum window.

## Measurement limits

The sampler uses macOS `proc_pid_rusage` physical footprint rather than `ps`
RSS. It retains raw per-process values and process start identity, CPU
nanoseconds, wakeups, and disk-I/O counters. Descendants are deduplicated. It
fails if a root process disappears or a selected process cannot be measured.
Process-set changes invalidate a stable-phase comparison; short-lived
processes between samples are not fully accounted for.

CPU counters are converted from Mach absolute time with the host timebase.
The macOS sampler test compares the result with a controlled child's own
process CPU clock, so a unit mismatch cannot silently underreport CPU use.

Summed physical footprint is an operational host metric, not a perfect
allocation ownership model for shared pages. Sampling itself and concurrent
host activity can perturb results. Avoid conclusions from differences below
run-to-run noise. Energy use requires an independently controlled power
measurement; lower CPU does not by itself prove lower energy consumption.

# Validation record

This record describes development validation on October 21, 2026 on an Apple
Silicon Mac running macOS 27.0.1. It is not an OrbStack performance acceptance
report. Sapphire remains experimental.

## Host and source checks

`scripts/check.sh` passed locally. It runs the Go host service race tests and
vet, guest tests and vet, Linux ARM64 guest cross-compilation, resource sampler
and comparison tests, native tests and sanitizers, and Swift tests.

- Native device, memory, transport, and malformed-input tests: 219,366
  assertions, also passing with AddressSanitizer and UndefinedBehaviorSanitizer.
- Swift: 12 core runtime tests and four app model tests.
- Resource measurement/comparison logic: seven tests, including macOS CPU
  counter calibration against a controlled process's own CPU clock.
- `scripts/build.sh` produced the CLI, engine, VMM, and `Sapphire.app`.
  Strict code signature verification passed for the app and standalone VMM.
- Installation into a disposable prefix passed, including the installed CLI's
  `doctor` and stopped-runtime status, sibling engine discovery, guest asset
  discovery, and verification of the installed VMM signature.

The native driver also passed an independent four-vCPU Linux fixture covering
SMP/PSCI, RTC, block read/write/flush/discard, Ethernet, a 2 MiB vsock exchange,
free-page reporting, parent-lifetime EOF, bounded resets, and shutdown.
The follow-up fixture's host physical footprint fell from 323.0 MiB with a
retained 128 MiB allocation to 171.0 MiB after release, with a 512 MiB guest
ceiling. These figures describe the native fixture, not the complete appliance
or an optimization comparison between builds.

## Functional integration

The integration harness is `scripts/smoke-runtime.py`. It snapshots the selected
engine, VMM, and guest assets into its own `/tmp` runtime and uses explicit
private Docker sockets. The host Docker context and existing OrbStack/Nash
workloads remain unchanged.

The full smoke checks cover:

- Direct Hypervisor VM boot and guest RPC readiness.
- On-demand Docker, a digest-pinned ARM64 image pull, and real execution.
- Named-volume persistence, published TCP HTTP forwarding and listener removal,
  Docker API inventory, and Compose lifecycle.
- A namespace machine with verified CPU and memory cgroup limits, a
  capacity-limited sparse disk, and persistent data.
- Docker stop draining workloads and allowing a subsequent start.
- Kubernetes node and service-account readiness, CoreDNS rollout, service DNS
  from a pod, workload logs/deletion, and stop cleanup that preserves an
  ordinary Docker container.
- VM stop/restart and container/machine data persistence across that restart.

The separate `guest/smoke-k8s-network.py` check passed on a fresh private VM
with the corrected guest kernel. It checked all three core deployments,
pod DNS, the Kubernetes API ClusterIP, an HTTP Service, ingress policy denial
and a selected-client allowance, and Kubernetes stop cleanup preserving an
ordinary container. It also checked the required mark, logging, limit, and
reject operations, bridge filtering, and the absence of policy-rule restore
and connection-tracking cleanup failures.

Earlier runs exposed missing kernel netfilter capabilities and errors in the
test HTTP fixture. Those failures were corrected before the resource captures.

The frozen inputs for the resource run `sapphire-smoke-5aptryb9` are:

| Input | SHA-256 |
| --- | --- |
| Host engine | `567d80049e4bc341a8dabb75a49d175483380a246730bb5f85ba9bb959bb7174` |
| Native VMM | `7a390305864d9239a133ee65e9d50a2117d81291d8d6de04865b31c2a0d5c021` |
| Guest kernel | `bd6f2b6ec7f083c120419b84c57a4dcb7750a5e0f20233662b824c356f032efe` |
| Guest initramfs | `473127c934bec32b17910acb707cdc5cc7469138f536f740a319c170d2cfdb85` |
| Guest root filesystem | `524853356f6c004b98c53f3e7cc4bc97a1c63a853c740038f29680a0aee678bc` |

The final packaged smoke, `sapphire-smoke-u4xdpa9c`, passed all 14 functional
checks with the same engine and guest assets. It used native VMM
`97b8a0afdd2c047fb08c90ba86de6c285f2e3eee34f985f6b181df421b89a1db`,
which adds optional self-memory diagnostics disabled by default. Strict
signature verification passed for the packaged app and VMM, and the private
runtime was confirmed stopped after the smoke. That pass did not recapture
resource windows; the resource figures below retain their earlier input hash.

## Resource interpretation

Sapphire's resource acceptance remains open. Sharing the VM mechanism with
OrbStack does not establish equivalent device internals or performance.

The complete-appliance smoke protocol uses two vCPUs, a 2048 MiB VM ceiling,
and a 32 GiB sparse data disk. Its machine has a one-CPU quota, a 1 GiB memory
limit, and an 8 GiB sparse disk. Each resource workload holds 256 MiB in tmpfs,
then unlinks it. Captures include the engine and VMM and exclude the UI.

The frozen-input run `sapphire-smoke-5aptryb9` passed all 15 functional checks
and recorded all nine 60-second resource windows. Every window retained the
same engine/VMM process identities. The figures below are diagnostic results
for the executable hashes listed above, before the subsequent native memory
investigation.

| Workload | Phase | P95 footprint (MiB) | Mean CPU (%) | P95 CPU (%) | Interrupt wakes/s |
| --- | --- | ---: | ---: | ---: | ---: |
| Container | Idle | 430.9 | 3.22 | 7.24 | 56.6 |
| Container | Allocated | 686.9 | 0.34 | 0.64 | 44.5 |
| Container | Released | 442.3 | 0.40 | 0.66 | 43.3 |
| Machine | Idle | 474.1 | 0.32 | 0.53 | 41.5 |
| Machine | Allocated | 728.8 | 0.37 | 0.59 | 41.3 |
| Machine | Released | 476.8 | 0.30 | 0.41 | 39.9 |
| Kubernetes | Idle | 2642.2 | 7.31 | 10.93 | 624.5 |
| Kubernetes | Allocated | 2843.0 | 7.15 | 10.45 | 605.5 |
| Kubernetes | Released | 2637.2 | 7.74 | 10.80 | 529.4 |

CPU percentages use 100% for one full core. All nine captures measured zero
idle wakeups; interrupt wakeups were nonzero as shown. Footprint is the macOS
physical-footprint accounting metric, including compressed/device charges,
not an RSS measurement or a count of unique resident DRAM bytes.

The first container window still contains changing background activity:
its first 20 seconds averaged 4.76% CPU and its last 20 seconds averaged
0.33%. It must not be presented as a stable warmed-idle baseline. Later
workload phases include earlier functional activity and retained caches;
their totals do not isolate incremental machine or Kubernetes overhead.

The Kubernetes host footprint exceeds the guest RAM ceiling. A separate
cached restart and region summary ruled out hundreds of MiB of visible C heap
payload as the explanation. A controlled Hypervisor fixture then reproduced
approximately 1025 MiB of footprint while the guest wrote a 512 MiB RAM
window. Host readback verified the guest writes. Releasing all but a protected
2 MiB range returned footprint to approximately 4.3 MiB over six cycles;
the excess charge also disappeared on Hypervisor unmapping/teardown.

This fixture did not demonstrate accumulating retained allocations across
those cycles. It shows that Hypervisor-associated accounting can exceed the
current guest RAM window, so the Kubernetes figures cannot be read as a count
of unique resident DRAM bytes. It does not explain every full-appliance
allocation or prove resource parity. A matched OrbStack comparison must retain
consistent accounting metrics, and host-pressure/cache behavior still needs
its own validation.

The development host also runs unrelated OrbStack/Nash work. Local captures
are shared-host diagnostics and cannot pass the comparator's exclusive
baseline requirement. Matched OrbStack captures, repeated runs, startup,
throughput/latency, storage, energy, machine density, and complete UI overhead
remain required by [the acceptance protocol](resource-acceptance.md).

Known performance work includes synchronous block I/O under the device lock,
copying in device paths, reclamation batching, guest page-cache behavior, and
a host-pressure policy. Free-page reporting reclaims allocator-free pages;
it does not evict Linux filesystem caches.

Machine tests cover ordinary workloads in the initial trusted-root mode.
Machine root retains privileged guest credentials and shares the network
namespace; the tests do not establish a security boundary against that root
user. Treat root access as administration of the shared Linux guest and match
this trust mode in any resource comparison.

The resource audit also removed two avoidable costs: machine lock entries now
cover active holders/waiters instead of accumulating historical names, and a
Docker event refresh uses one published-port snapshot instead of inspecting
every running container. Short failed event sessions now back off to a
30-second ceiling; resetting the backoff requires a healthy session.

## Reproduce

```sh
scripts/check.sh
scripts/build.sh --with-guest
python3 scripts/smoke-runtime.py --with-k8s --resource-seconds 120 --keep
```

To rerun the additional network-policy check, use only the disposable runtime
retained by that command. Substitute its printed path for `RUNTIME`:

```sh
runtime=RUNTIME
SAPPHIRE_HOME="$runtime" SAPPHIRE_VMM="$runtime/bin/sapphire-vmm" \
  "$runtime/bin/sapphire-engine" start --cpus 2 --memory-mib 2048 --data-gib 32
python3 guest/smoke-k8s-network.py --disposable-guest --home "$runtime"
```

The network-policy check stops that test VM when finished.

The guest build pins Linux 6.18.57, Alpine 3.23.6, and k3s
v1.37.1+k3s1, with downloaded artifacts checked by digest. Generated assets,
private runtime data, and raw local records are excluded from Git. Each
functional record retains the actual executable and asset hashes.

Interactive machine PTYs, Rosetta, host filesystem sharing, macOS VMs,
arbitrary guest kernels, UDP published forwarding, and complete OrbStack
filesystem/network compatibility are outside this initial validation.

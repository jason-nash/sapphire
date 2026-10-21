# Sapphire

An open-source Linux runtime for Apple Silicon, with a native macOS app and
CLI. Sapphire uses its own C VMM directly on Hypervisor.framework and runs
containers, lightweight Linux machines, and Kubernetes in one shared Linux
kernel.

**Development status:** experimental. This is an independent reconstruction
guided by documented architecture and local artifact evidence. It is not a
complete OrbStack replacement. Similar or lower resource consumption than
OrbStack is an acceptance target; it has not been demonstrated.

## Architecture

- Direct Hypervisor.framework VMM, ARM64 Linux boot, GICv3, and virtio devices.
- A Go host engine with private Unix control/Docker sockets and an
  unprivileged userspace network.
- A Linux guest agent managing namespace/cgroup machines and on-demand
  Docker and k3s services.
- A Swift CLI and native SwiftUI app. Data refresh is explicit and tied to
  application activity; there is no continuous UI refresh loop.
- Independently built Linux assets. OrbStack binaries and guest images are
  not part of the project.

Read [the architecture](docs/architecture.md),
[the investigation evidence](docs/orbstack-evidence.md), and
[the resource acceptance criteria](docs/resource-acceptance.md).
The [validation record](docs/validation.md) separates verified behavior from
remaining acceptance work.

## Requirements

The host runtime requires an Apple Silicon Mac running macOS 15 or later,
Xcode command-line tools with the macOS SDK, Swift 6, and Go 1.26 or later.
Docker CLI and Compose are required for their corresponding CLI operations.
Building guest assets requires an existing Docker builder. The host VMM does
not depend on that builder after assets have been produced.

## Build

```sh
scripts/build.sh
```

This produces `dist/bin/sapphire`, `sapphire-engine`, `sapphire-vmm`, and
`dist/Sapphire.app`. To build the independent guest assets as well:

```sh
scripts/build.sh --with-guest
```

This guest build includes the pinned k3s binary; Kubernetes still starts only
when requested. Use `guest/build.sh` directly for an image without Kubernetes.

Keep generated guest images and private runtime data out of source control.
The native driver is ad-hoc signed with the hypervisor entitlement during the
build. Distributed releases would additionally require an appropriate signing
and notarization process.

## Install and use

```sh
scripts/install.sh
export PATH="$HOME/.local/bin:$PATH"
sapphire doctor
sapphire start --cpus 4 --memory-mib 4096
sapphire docker run --rm hello-world
sapphire create dev --distribution alpine --cpus 2 --memory 2 --disk 16
sapphire exec dev -- uname -a
sapphire k8s start
sapphire k8s kubeconfig
sapphire k8s stop
sapphire stop
```

These commands are the intended interface; consult the validation record for
the current build's verified behavior. Resource limits on machines are CPU,
memory, and sparse disk capacity. Machines share the VM's kernel.
Run trusted machine workloads only: machine root currently has administrative
authority in the shared Linux guest, including its storage and other workloads.
Untrusted machine isolation is not implemented.

`SAPPHIRE_HOME` selects an independent runtime directory, defaulting to
`~/.sapphire`. Guest assets belong in its `assets/` directory. All Sapphire
Docker commands explicitly use that runtime's Docker socket. Installing and
using Sapphire preserves the selected global Docker context.

The app bundle can remain in `dist/` or be placed in an application directory.
It expects the packaged engine and VMM beside its executable. It presents real
runtime status and errors; the separately invoked offscreen preview mode uses
labeled demonstration data.

## Validation

```sh
scripts/check.sh
```

This checks the Go services, cross-compiles the Linux guest, checks resource
benchmark logic, and, on Apple Silicon macOS, builds/tests the C VMM and Swift
targets. Linux CI exercises Linux-specific guest code; macOS CI covers the
host runtime. Hardware boot, Docker, machines, Kubernetes, and matched
resource measurements require integration checks beyond unit/build CI.

After building independent guest assets, exercise a separate, disposable VM:

```sh
python3 scripts/smoke-runtime.py --with-k8s
```

This checks real containers, named volumes, published HTTP ports, Compose,
machine limits and persistence, Kubernetes, and VM restart. It uses a fresh
runtime directory and its own Docker socket. It stops its VM on completion;
failed runs retain logs, and functional records are written under
`benchmarks/results/`. The Kubernetes check requires a host `kubectl`.

Add `--resource-seconds 120 --keep` to capture host physical footprint, CPU,
and wakeups while each workload is idle, holds a 256 MiB tmpfs allocation, and
has released it. These sequential, shared-host smoke observations help find
regressions; they cannot establish comparative performance or OrbStack parity.

See [the resource protocol](docs/resource-acceptance.md) before interpreting
benchmark output. A busy OrbStack instance cannot provide an isolated
baseline.

## Compatibility boundaries

Interactive machine PTYs, Rosetta, host directory sharing, macOS VMs, arbitrary
guest kernels, and OrbStack's complete filesystem/network integration remain
outside the initial implementation. Unsupported options fail explicitly.
Feature presence in source does not imply a completed end-to-end test.

## License

Sapphire's original code is MIT licensed. Linux, Alpine packages, Docker,
k3s, gVisor, and other dependencies retain their own licenses. A guest build
must retain package/license information and corresponding source obligations.

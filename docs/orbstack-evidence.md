# OrbStack investigation ledger

The inspected local installation was OrbStack 2.2.4, bundle version 21120, on
Apple Silicon on October 21, 2026. REA 6.7.0 performed static artifact
inspection. A separate read-only process sample provided runtime evidence.
Sapphire contains independently written implementation code and independently
built guest assets.

## Artifact identity

The VM helper was:

```text
/Applications/OrbStack.app/Contents/Frameworks/OrbStack Helper.app/Contents/MacOS/OrbStack Helper
SHA-256: 5814aac5726ae06a437de06ad1ed41337648a5e8be288c1ee245574c51261d24
```

| Observation | Evidence ID / source | Boundary |
| --- | --- | --- |
| App bundle and shipped resource inventory | `ev_4e385fe0d9cabc993e485e972969b264f19405236f9513dda461fe4fec6ab04b` | Inventory establishes shipped files, not execution |
| Main executable Mach-O metadata | `ev_4e6be427dc3f6e2791d77e29585b526f89383da5050b4b4ab889d0c6ed749331` | Static metadata/imports |
| Version and bundle metadata | `ev_fd4d28c72df065640079c7a941acd117ac99ad423c02156cdfe790071c2beab9` | Inspected version only |
| Helper imports direct VM mapping and vCPU Hypervisor operations | `ev_fafe707e9cfe0fe34a23b1f178208afc640597f3118e7354980f0b7072726512` | Imports alone do not select the active backend |
| Running vCPU stacks execute `Hv::Vcpu::run()` in Hypervisor | Local read-only runtime sample, retained privately as `/tmp/sapphire-orbstack-vmgr-sample.txt` | Observed running helper; sample is not decompiled code |
| Embedded Go module metadata includes gVisor dependencies | `ev_d857599c5c1f1c6313a4228dc3cfd262b0618994849a0012ae8d0a8b1312aee5` | Dependency metadata does not prove every dependency is used on a particular path |
| Rust dependency audit section absent | `ev_f04b75647c60ab83d69bf219d02e3e88cf292e1a2dade97bb22150a8ab6ec722` | No dependency graph can be recovered from this missing section |

The helper also imports Virtualization.framework. This does not contradict
the observed direct Hypervisor execution, and it does not establish which
other features use Virtualization.framework.

Shipped strings identify virtio device implementation source paths for block,
network, vsock, balloon, and filesystem-related code. This supports a device
model investigation; it does not establish complete source code, precise
algorithms, or an upstream VMM identity.

## Conclusions

- **Observed:** the running VM helper uses Apple's Hypervisor implementation.
  Static imports include the direct VM/vCPU API.
- **Published architecture:** OrbStack documents one shared Linux kernel for
  Docker and lightweight Linux machines.
- **Sapphire design choice:** implement an independent userspace VMM directly
  against Hypervisor.framework with virtio devices and one shared guest.
- **Unknown:** OrbStack's exact device internals, active networking paths,
  memory reclamation policy, upstream lineage, scheduling, and complete
  filesystem implementation.
- **Unmeasured:** resource and performance parity. Shared architecture is
  insufficient evidence for this claim.

The investigation did not recover original OrbStack source. It does not
establish that OrbStack is based on libkrun or a particular upstream fork.
Source-path resemblance and unavailable Rust metadata are insufficient for
that conclusion.

## Public primary references

- OrbStack architecture: `https://docs.orbstack.dev/architecture`
- Apple Hypervisor framework: `https://developer.apple.com/documentation/hypervisor`
- Linux free-page reporting: `https://www.kernel.org/doc/html/latest/mm/free_page_reporting.html`
- K3s resource profiling: `https://docs.k3s.io/reference/resource-profiling`

Private full REA reports and runtime samples are excluded from the repository.
They contain local paths and unnecessary artifact details. This ledger retains
the evidence identities and the limits of the conclusions.

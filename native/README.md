# Sapphire native ARM64 VMM

`build/sapphire-vmm` is a small C VMM that calls macOS Hypervisor.framework
directly. It creates and maps its own VM, uses the framework's GICv3, and runs
one vCPU on each owning pthread. It does not embed another VM manager.

The build targets Apple Silicon and macOS 15 or newer. An Apple SDK containing
the native GIC APIs is required.

## Build and test

```sh
make -C native all
make -C native test sanitize
make -C native probe smoke
make -C native memory-test
```

`all` links Hypervisor.framework and ad-hoc signs the executable with
`com.apple.security.hypervisor`. `probe` creates and tears down only its own
VM, GIC, and vCPU. It prints a JSON capability record.

The offline tests exercise the FDT, descriptor validation, malformed rings,
index wraparound, block reads/writes/flush/discard and maximum-sized requests,
Ethernet, RNG, vsock credits,
RTC time/alarms, the explicit disk-initialization flag, and partial-host-page
reclamation boundaries. The sanitizer target runs those
tests with AddressSanitizer and UndefinedBehaviorSanitizer.
Vsock regressions use real kqueue/socket pairs to check zero-credit disconnects,
idle waits, credit recovery, and half-close delivery of queued output.

`smoke` assembles small test payloads with the installed Clang. These are
synthetic Image-shaped guests, not Linux. They verify real EL1 execution, FDT
in x0, PL011, PSCI CPU_ON/CPU_OFF/SYSTEM_OFF, SMP, native WFI, and shutdown on
parent-pipe EOF. A reset-only guest verifies that descriptors survive two
successive reboots and that the third rapid reset exits with an error.

`memory-test` creates only its own HV/GIC/vCPU and 512 MiB RAM mapping. Six
cycles write every workload page, verify those writes from EL1, write back from
EL1, and report free ranges through the production reclamation callback.
The last 2 MiB remains live and its canaries must survive every report.
Released task-footprint charges must stay bounded across cycles. Self
TASK_VM_INFO and RAM-region records are emitted before/after allocation,
at loaded/released phases, and through vCPU/HV/RAM teardown. The test waits
between phases because global host VM statistics are cached; those additional
global counters are shared-host context, not isolated memory-use measurements.

The independent Linux test takes explicit local Alpine inputs and Go for its
small Linux vsock test program:

```sh
python3 native/tests/linux_integration.py \
  --kernel-apk native/build/reference/linux-virt.apk \
  --base-initrd native/build/reference/initramfs-virt \
  --cpus 2 \
  --memory-diagnostics \
  --footprint-probe benchmarks/build/footprint
```

It builds its fixtures under `native/build/linux-test/`, boots Linux, requires
its initial UTC clock to match the host within 30 seconds, tests block
read/write/flush/discard, answers raw Ethernet ARP/ICMP, echoes 2 MiB over
vsock, allocates/frees 128 MiB, allows page reporting to run, and powers off.
The root/data disks are test fixtures owned by that test. The test does not
download inputs or access another VM. Console, host log, and JSON samples remain
in that directory. The footprint probe is optional and is used when available;
the VMM has no dependency on it. These are native-process measurements, not a
container/Kubernetes benchmark or a comparative resource guarantee.

## Driver API

```sh
native/build/sapphire-vmm \
  --kernel /absolute/path/Image \
  --initrd /absolute/path/initrd.gz \
  --root-disk /absolute/path/rootfs.squashfs \
  --data-disk /absolute/path/data.ext4 \
  --memory-mib 1024 \
  --cpus 2 \
  --net-fd 3 \
  --vsock /absolute/path/guest.vsock \
  --console /absolute/path/console.log \
  --parent-fd 5
```

All path flags and `--net-fd` are required for a normal run. Memory defaults to
1024 MiB, CPUs to 2. Memory must be at least 128 MiB, and CPU count must be
between 1 and the host Hypervisor limit (also bounded at 256). Excess address
space requests fail before the VM runs. `--parent-fd` is optional and defaults
to disabled.

- `--kernel`: a raw, little-endian ARM64 Linux Image with its standard header.
  The loader honors `text_offset` and `image_size`. It does not accept compressed
  kernels or EFI zboot executables.
- `--initrd`: a Linux initramfs, normally gzip-compressed cpio. It supplies
  `/init` and handles mounting the root and data filesystems.
- `--root-disk`: an existing regular file, opened read-only with a shared lock.
  A partial final 512-byte sector is padded with zeroes for reads.
- `--data-disk`: an existing regular file with a size divisible by 512, opened
  read/write with an exclusive nonblocking lock. The driver does not create or
  format it.
- `--net-fd`: a connected inherited AF_UNIX SOCK_DGRAM endpoint. Each datagram
  is one complete Ethernet frame, without a virtio header or length prefix.
  The NIC MAC is `5a:94:ef:e4:0c:ee`. The host supplies DHCP, NAT and other network
  services. Congestion may drop packets; malformed/oversized frames are dropped.
- `--vsock`: a new UNIX stream listener path. Existing paths are rejected.
  It is created with mode 0600 and removed on orderly teardown, provided its
  inode still matches the one created by this process.
- `--console`: a file opened for append, receiving PL011 output. It is a log,
  not an interactive input channel.
- `--parent-fd`: an inherited read pipe. The controller retains the writer.
  Kqueue EOF or an error stops the VM and all vCPU threads. The same descriptor
  remains active through guest-requested resets. There is no PID polling.
- `--initialize-data-disk`: an optional switch, false by default. It adds the
  zero-length `/chosen/sapphire,initialize-data-disk` FDT property, allowing the
  guest initramfs to initialize a disk the controller has just created. Native
  does not infer whether a disk is new or format it. The controller/operator
  must supply the flag only for a disk it intends to initialize.
- `--memory-diagnostics`: an optional switch, false by default. It emits
  `sapphire-memory: ` followed by JSON to stderr at RAM/HV lifecycle boundaries.
  While enabled, SIGUSR1 requests another snapshot through kqueue. Records
  include self TASK_VM_INFO components and the RAM mapping's region count,
  resident/swapped page counts, object references and shadow depths. The first
  16 region details are included; totals cover the bounded walk and indicate
  whether it completed. The diagnostic never attaches to another task and
  adds no periodic polling. Global host counters, when available, are context
  only. Without this switch, SIGUSR1 retains its normal signal behavior.

Normal shutdown returns 0. Startup/runtime failures return 1. Argument errors
return 64. SIGINT, SIGTERM and SIGHUP stop the VM. Network peer termination also
stops it. Guest PSCI SYSTEM_RESET tears down and recreates this VM in the same
process; it does not require the controller to relaunch the executable.
The network and parent descriptors are duplicated once into process-owned
handles that survive each VM teardown. Three consecutive resets with less than
30 seconds of uptime each stop the process with exit status 1 and a diagnostic
pointing to the console log. A reset after at least 30 seconds clears that
counter. This permits intentional reboots while bounding faulty-image storms.

`--probe [--cpus N]` requires no images or descriptors. It checks Hypervisor
access and reports the host vCPU limit plus implemented GIC/reporting support.
It does not execute Linux or prove reclamation on its own.

## Vsock bridge

The host connects to the UNIX listener and sends exactly:

```text
CONNECT 1024\n
```

The port can be any nonzero uint32. The driver sends a virtio-vsock stream
REQUEST from CID 2 and an ephemeral host port to CID 3 and the requested guest
port. Only after the guest responds with RESPONSE does the local socket receive
`OK\n`. Subsequent bytes form a bidirectional stream. A reset, refusal or timeout
closes the local socket. It does not fabricate an OK response.

Ports 1024 (control), 1025 (Docker proxy), and 1026 (TCP forwarding) use the
same mechanism; the VMM does not interpret their application protocols. A
64-connection bound, bounded queues, per-direction credits, half-close
propagation, and a 30-second connect deadline limit buffering. Guest-initiated
connections to arbitrary host vsock services are reset; there is no host
AF_VSOCK service listener.

Local socket read and write filters remain armed when credit is exhausted or
output is empty. Those idle filters use EV_CLEAR; active data/output filters use
level readiness. This detects disconnects without polling or a repeated-ready
loop. Read EOF waits for preceding input to be forwarded; a write half-close
retains queued guest output until delivered. Full disconnects release the
client's descriptor, buffer, and slot.

## Devices and execution

The generated FDT describes RAM at `0x40000000`, a GICv3 distributor at
`0x08000000`, redistributors at `0x10000000`, PL011 at `0x09000000`, the
architectural timer, a PL031 RTC at `0x09010000` (SPI 34), PSCI via HVC, and these
modern virtio-mmio devices:

| Slot | Address | SPI | Device |
| --- | --- | --- | --- |
| 0 | `0x0a000000` | 48 | Root block, read-only (`vda`) |
| 1 | `0x0a001000` | 49 | Data block, read/write (`vdb`) |
| 2 | `0x0a002000` | 50 | Ethernet |
| 3 | `0x0a003000` | 51 | RNG |
| 4 | `0x0a004000` | 52 | Vsock, guest CID 3 |
| 5 | `0x0a005000` | 53 | Balloon free-page reporting |

Every transport uses split queues of at most 256 entries. Indirect descriptors,
packed rings, notification suppression with EVENT_IDX, checksum/GSO offloads,
and multi-queue networking are not offered. Descriptor chains are copied and
validated for guest-RAM bounds, byte limits, direction order, indices and loops
before host I/O. Block devices advertise at most 128 data segments of 64 KiB
each, allowing up to 8 MiB of data plus the 16-byte header and 1-byte status.
Those limits are checked separately from the ring envelope. Read/write requests
outside the segment limits return IOERR; malformed/oversized chains require a
device reset. Block I/O uses a fixed 16 KiB scratch buffer, without allocating a
request-sized buffer. Other ordinary chains are bounded at 1 MiB; free-page
reports at 256 MiB per chain.

The primary starts at EL1h with the MMU disabled, masked interrupts, x0 pointing
to the generated FDT, and x1-x3 zero. Secondary CPUs are created before any vCPU
runs, then sleep on a condition variable until PSCI CPU_ON. CPU_OFF returns to
that wait. GICv3 handles virtual interrupts, timers, and WFI in Hypervisor;
there is no periodic vCPU polling loop. The main thread waits in kqueue for
network, listener/client, signal, parent-pipe, device-wakeup, or programmed RTC
alarm events.
Only vsock queue/status work wakes that main loop from virtio writes; block,
network TX, RNG, IRQ acknowledgements, and queue-address writes do not wake it
unconditionally. UART interrupt updates and virtio IRQ acknowledgements call
the GIC only when their asserted level changes.

The RTC starts with host CLOCK_REALTIME Unix seconds. Linux enables its counter
during PL031 probe; load-register writes change only a VM-local clock offset.
Control, match, mask, raw/masked interrupt status, interrupt clear, and PrimeCell
identification registers are implemented. Alarms use a deadline for kqueue's
blocking wait, with no periodic clock polling. Linux needs RTC_DRV_PL031 and
RTC_HCTOSYS enabled to establish wall time before userspace/TLS starts.

Boot arguments are:

```text
console=ttyAMA0 earlycon=pl011,mmio32,0x09000000 rdinit=/init panic=1
```

Linux needs ARM GICv3, PL011 console, virtio-mmio, block, network, RNG, vsock,
and balloon/reporting drivers built in or available to the initramfs.

## Memory and disk reclamation

Both disk image descriptors request Darwin `F_NOCACHE` to avoid retaining a
second long-lived cache of data already cached by Linux. A rejected request is
logged and falls back to cached I/O. Guest FLUSH and writable-disk teardown
still call `fsync`; this is not a write-through or power-loss guarantee.
Small/unaligned reads and writes, padded read-only tails, maximum-size requests,
and discard are tested with this setting. Host-cache reduction and its latency
tradeoff have not been benchmarked against the cached path or another runtime.

RAM is an anonymous demand-backed mmap; it is not pre-cleared or locked.
Balloon feature 5 (free-page reporting) is implemented. No optional statistics
or free-page-hint queues are advertised, so the reporting queue follows
inflate/deflate at queue index 2. The inflation target remains zero; this is
free-page reporting, not a controller-driven memory resize interface.

For each reported range, the driver validates guest bounds, rounds its start
up and its end down to the actual host page size, and skips partial boundary
pages. On a 16 KiB host, an isolated 4 KiB guest report cannot reclaim any page.
Separate partial descriptors are not merged. It never rounds outward.

Linux holds the reported pages outside its allocator until used completion.
While holding the device lock, the VMM removes that complete range's Hypervisor
mapping, calls MADV_FREE, replaces only its owned anonymous backing at the same
host address, then restores the Hypervisor mapping before returning the report.
Failures stop the VM instead of acknowledging successful reclamation.
Shutdown logs distinguish total reported bytes, replaced mapping bytes and
skipped boundary bytes. Reported/replaced counts can include the same page on
multiple reports and are not an RSS/footprint gauge.

Guest RAM size is not a ceiling on the host task's physical-footprint ledger.
On the tested macOS 27 host, the owned 512 MiB churn fixture charged roughly
512 MiB after host writes and roughly 1024 MiB after the vCPU touched the same
pages. Removing the HV mapping removed that additional charge while the
populated host RAM mapping stayed intact. Free-page reports returned the
fixture to a small, stable released charge across cycles. This supports HV
mapping/alias accounting rather than cumulative backing retention in that
fixture; it does not establish unique DRAM use or resource parity. An ordinary
MAP_SHARED experiment did not remove the extra charge, so private backing is
retained. No purgeable-accounting policy or automatic guest-cache eviction is
used. Host-pressure effects, guest cache policy and whole-runtime comparisons
remain separate measurements.

The writable block device advertises DISCARD with up to 32 ranges and 65536
sectors per range, using a conservative alignment of at least one host page.
All ranges and flags are validated before any mutation. Complete aligned
interiors are deallocated with Darwin F_PUNCHHOLE; partial boundary blocks are
retained. The read-only disk does not offer DISCARD. Unsupported host
filesystems return a device error/unsupported result. WRITE_ZEROES is not
offered. Guest filesystems must issue discard/fstrim; deleting guest files
alone does not promise host-space reclamation.

Remaining scope limits include interactive serial input, CPU_SUSPEND, PMU,
PCI/GPU/USB, migration/snapshots, host filesystem sharing, and disk zeroing
offloads. Full container/machine/Kubernetes functionality and resource
comparison require the independently supplied guest and host controller.

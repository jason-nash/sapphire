#!/usr/bin/env python3
"""Boot a public Alpine kernel with our test init, modules, and vsock echo guest.

Inputs are explicit local files. No VM other than the spawned Sapphire child is
controlled. All fixtures and logs are kept under native/build/linux-test/.
"""
import argparse
import gzip
import io
import json
import os
from pathlib import Path
import select
import re
import signal
import socket
import struct
import subprocess
import tarfile
import threading
import time
import zlib

BASE = Path(__file__).resolve().parent.parent
WORK = BASE / "build/linux-test"

INIT = b"""#!/bin/busybox sh
BB=/bin/busybox
$BB mount -t proc proc /proc
$BB mount -t sysfs sys /sys
$BB mount -t devtmpfs dev /dev
$BB modprobe virtio_mmio
$BB modprobe virtio_blk
$BB modprobe virtio_net
$BB modprobe virtio_balloon
$BB modprobe virtio_rng
$BB modprobe vmw_vsock_virtio_transport
echo SAPPHIRE_LINUX_INIT_OK
echo SAPPHIRE_CLOCK_UNIX=$($BB date +%s)
$BB cat /sys/devices/system/cpu/online
$BB dd if=/dev/vda of=/root-read bs=512 count=1
$BB head -c 21 /root-read
echo
$BB dd if=/dev/urandom of=/dev/vdb bs=4096 count=16
$BB sync
$BB ifconfig eth0 192.0.2.2 netmask 255.255.255.0 up
$BB ping -c 1 -W 3 192.0.2.1 && echo SAPPHIRE_NET_OK
/vsock-guest && echo SAPPHIRE_VSOCK_DONE
$BB mkdir /ramtmp
$BB mount -t tmpfs tmpfs /ramtmp
$BB dd if=/dev/urandom of=/ramtmp/workload bs=1048576 count=128
echo SAPPHIRE_MEMORY_LOADED
$BB sleep 2
$BB rm /ramtmp/workload
echo SAPPHIRE_MEMORY_FREED
$BB sleep 8
echo SAPPHIRE_MEMORY_RELEASED
$BB poweroff -f
"""


def cpio_entry(name, data, mode):
    name = name.encode() + b"\0"
    fields = [1, mode, 0, 0, 1, 0, len(data), 0, 0, 0, 0, len(name), 0]
    raw = b"070701" + b"".join(f"{n:08x}".encode() for n in fields) + name
    raw += bytes(-len(raw) % 4)
    raw += data
    raw += bytes(-len(raw) % 4)
    return raw


def fixtures(kernel_apk, initrd):
    WORK.mkdir(parents=True, exist_ok=True)
    subprocess.run(
        ["go", "build", "-trimpath", "-o", str(WORK / "vsock-guest"),
         str(BASE / "tests/vsock_guest.go")],
        env={**os.environ, "GOOS": "linux", "GOARCH": "arm64", "CGO_ENABLED": "0"},
        check=True,
    )
    archive = bytearray()
    data = gzip.decompress(kernel_apk.read_bytes())
    with tarfile.open(fileobj=io.BytesIO(data), ignore_zeros=True) as tar:
        for member in tar:
            if member.name.endswith("/vmlinuz-virt"):
                raw = tar.extractfile(member).read()
                offset = raw.find(b"\x1f\x8b\x08")
                image = zlib.decompressobj(31).decompress(raw[offset:])
                assert image[56:60] == b"ARMd"
                (WORK / "Image").write_bytes(image)
            if member.name.startswith("lib/modules/") and (member.isdir() or member.isfile()):
                data = tar.extractfile(member).read() if member.isfile() else b""
                mode = member.mode | (0o040000 if member.isdir() else 0o100000)
                archive += cpio_entry(member.name, data, mode)
    archive += cpio_entry("init", INIT, 0o100755)
    archive += cpio_entry("vsock-guest", (WORK / "vsock-guest").read_bytes(), 0o100755)
    archive += cpio_entry("TRAILER!!!", b"", 0)
    (WORK / "initrd.gz").write_bytes(initrd.read_bytes() + gzip.compress(archive))
    with (WORK / "root").open("wb") as f:
        f.write(b"SAPPHIRE_ROOT_READ_OK")
        f.truncate(65536)
    with (WORK / "data").open("wb") as f:
        f.truncate(64 * 1024 * 1024)


def checksum(data):
    if len(data) & 1:
        data += b"\0"
    result = sum(struct.unpack(f"!{len(data) // 2}H", data))
    while result >> 16:
        result = (result & 65535) + (result >> 16)
    return (~result) & 65535


def network(peer, stop, counts):
    host_mac = b"\x02\x53\x41\x50\x00\x02"
    host_ip = socket.inet_aton("192.0.2.1")
    while not stop.is_set():
        if not select.select([peer], [], [], 0.1)[0]:
            continue
        frame = peer.recv(65536)
        if len(frame) < 14:
            continue
        if frame[12:14] == b"\x08\x06" and len(frame) >= 42 and frame[20:22] == b"\0\1":
            if frame[38:42] != host_ip:
                continue
            arp = b"\0\1\x08\0\x06\x04\0\2" + host_mac + host_ip + frame[22:32]
            peer.send(frame[6:12] + host_mac + b"\x08\x06" + arp)
            counts["arp"] += 1
        elif frame[12:14] == b"\x08\x00" and len(frame) >= 42:
            ihl = (frame[14] & 15) * 4
            if frame[23] != 1 or frame[14 + ihl] != 8:
                continue
            ip = bytearray(frame[14:14 + ihl])
            ip[12:16], ip[16:20] = ip[16:20], ip[12:16]
            ip[10:12] = b"\0\0"
            ip[10:12] = struct.pack("!H", checksum(bytes(ip)))
            length = struct.unpack_from("!H", ip, 2)[0]
            icmp = bytearray(frame[14 + ihl:14 + length])
            icmp[0] = 0
            icmp[2:4] = b"\0\0"
            icmp[2:4] = struct.pack("!H", checksum(bytes(icmp)))
            peer.send(frame[6:12] + host_mac + b"\x08\x00" + ip + icmp)
            counts["icmp"] += 1


def echo_test():
    with socket.socket(socket.AF_UNIX, socket.SOCK_STREAM) as stream:
        stream.settimeout(10)
        stream.connect(str(WORK / "vsock"))
        stream.sendall(b"CONNECT 1024\n")
        ack = bytearray()
        while not ack.endswith(b"\n"):
            piece = stream.recv(1)
            assert piece, "EOF before CONNECT reply"
            ack += piece
        assert ack == b"OK\n", ack
        payload = os.urandom(2 * 1024 * 1024)
        errors = []

        def send():
            try:
                stream.sendall(payload)
                stream.shutdown(socket.SHUT_WR)
            except Exception as exc:
                errors.append(str(exc))

        writer = threading.Thread(target=send)
        writer.start()
        received = bytearray()
        while len(received) < len(payload):
            piece = stream.recv(65536)
            assert piece, f"stream truncated at {len(received)}"
            received += piece
        writer.join(timeout=5)
        assert not writer.is_alive() and not errors, errors
        assert received == payload, "vsock echo mismatch"
        return len(received)


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--kernel-apk", type=Path, required=True)
    parser.add_argument("--base-initrd", type=Path, required=True)
    parser.add_argument("--cpus", type=int, default=2)
    parser.add_argument("--footprint-probe", type=Path)
    parser.add_argument("--memory-diagnostics", action="store_true")
    args = parser.parse_args()
    fixtures(args.kernel_apk, args.base_initrd)
    for path in [WORK / "console", WORK / "host.log"]:
        path.unlink(missing_ok=True)
    assert not (WORK / "vsock").exists(), "another test already owns this socket"
    peer, guest = socket.socketpair(socket.AF_UNIX, socket.SOCK_DGRAM)
    stop = threading.Event()
    counts = {"arp": 0, "icmp": 0}
    worker = threading.Thread(target=network, args=(peer, stop, counts))
    worker.start()
    probe = args.footprint_probe or BASE.parent / "benchmarks/build/footprint"
    result = {"cpus": args.cpus, "rss_kib": [], "process_samples": []}
    with (WORK / "host.log").open("wb") as log:
        vm = subprocess.Popen([
            str(BASE / "build/sapphire-vmm"), "--kernel", str(WORK / "Image"),
            "--initrd", str(WORK / "initrd.gz"), "--root-disk", str(WORK / "root"),
            "--data-disk", str(WORK / "data"), "--memory-mib", "512",
            "--cpus", str(args.cpus), "--net-fd", str(guest.fileno()),
            "--vsock", str(WORK / "vsock"), "--console", str(WORK / "console"),
        ] + (["--memory-diagnostics"] if args.memory_diagnostics else []),
            pass_fds=[guest.fileno()], stderr=log)
        try:
            started = time.monotonic()
            deadline = time.monotonic() + 45
            echoed = False
            sampled_stages = set()
            while vm.poll() is None and time.monotonic() < deadline:
                console = (WORK / "console").read_text(errors="replace") if (WORK / "console").exists() else ""
                if not echoed and "SAPPHIRE_VSOCK_READY" in console:
                    result["echo_bytes"] = echo_test()
                    echoed = True
                rss_result = subprocess.run(["ps", "-o", "rss=", "-p", str(vm.pid)],
                                            text=True, capture_output=True)
                rss = rss_result.stdout
                stage = "shutdown" if "SAPPHIRE_MEMORY_RELEASED" in console else (
                    "released" if "SAPPHIRE_MEMORY_FREED" in console else (
                        "loaded" if "SAPPHIRE_MEMORY_LOADED" in console else "boot"))
                if (args.memory_diagnostics and "SAPPHIRE_LINUX_INIT_OK" in console
                        and stage not in sampled_stages):
                    try:
                        os.kill(vm.pid, signal.SIGUSR1)
                    except ProcessLookupError:
                        pass
                    sampled_stages.add(stage)
                if rss.strip():
                    result["rss_kib"].append({"stage": stage, "rss": int(rss)})
                if probe.is_file():
                    sampled = subprocess.run([str(probe.resolve()), str(vm.pid)],
                                              text=True, capture_output=True)
                    if sampled.returncode == 0:
                        for sample in json.loads(sampled.stdout):
                            if sample["pid"] == vm.pid:
                                sample["stage"] = stage
                                sample["elapsed_s"] = time.monotonic() - started
                                result["process_samples"].append(sample)
                time.sleep(0.1)
            if vm.poll() is None:
                raise AssertionError("guest did not complete in 45 seconds")
            assert vm.returncode == 0, vm.returncode
            console = (WORK / "console").read_text(errors="replace")
            for marker in ["SAPPHIRE_LINUX_INIT_OK", "SAPPHIRE_ROOT_READ_OK",
                           "SAPPHIRE_NET_OK", "SAPPHIRE_VSOCK_DONE",
                           "SAPPHIRE_DISCARD_OK", "SAPPHIRE_MEMORY_RELEASED"]:
                assert marker in console, marker
            clock = re.search(r"SAPPHIRE_CLOCK_UNIX=(\d+)", console)
            assert clock, "guest did not report its boot clock"
            result["guest_unix_seconds"] = int(clock.group(1))
            result["host_unix_seconds"] = int(time.time())
            assert abs(result["host_unix_seconds"] - result["guest_unix_seconds"]) < 30, (
                "guest RTC did not initialize the system clock", result)
            host = (WORK / "host.log").read_text()
            assert "needs reset" not in host and "unhandled" not in host, host
            assert "reclaimed_mapping_bytes=0 " not in host, host
            assert counts["arp"] and counts["icmp"], counts
            result["network"] = counts
            result["memory_diagnostics"] = [
                json.loads(line.removeprefix("sapphire-memory: "))
                for line in host.splitlines() if line.startswith("sapphire-memory: ")
            ]
            if args.memory_diagnostics:
                phases = {sample["stage"] for sample in result["memory_diagnostics"]}
                assert {"requested", "after-hv-unmap", "after-hv-destroy",
                        "after-memory-destroy"} <= phases, phases
            print(host, end="")
            print(json.dumps(result, indent=2))
            (WORK / "result.json").write_text(json.dumps(result, indent=2) + "\n")
            print("PASS: Linux init and UTC clock, block read/write/flush/discard, Ethernet, "
                  "2 MiB vsock echo, balloon reporting")
        finally:
            if vm.poll() is None:
                vm.terminate()
                try:
                    vm.wait(timeout=5)
                except subprocess.TimeoutExpired:
                    vm.kill()
                    vm.wait()
            stop.set()
            worker.join(timeout=2)
            peer.close()
            guest.close()


if __name__ == "__main__":
    main()

#!/usr/bin/env python3
"""Execute only our small synthetic guest to test HV/GIC/EL1/PSCI/SMP."""
import argparse
import os
from pathlib import Path
import socket
import struct
import subprocess
import tempfile
import time


def text_section(obj):
    data = obj.read_bytes()
    assert data[:6] == b"\x7fELF\x02\x01"
    shoff = struct.unpack_from("<Q", data, 40)[0]
    shentsize, shnum, shstrndx = struct.unpack_from("<HHH", data, 58)
    sections = [
        struct.unpack_from("<IIQQQQIIQQ", data, shoff + i * shentsize)
        for i in range(shnum)
    ]
    names = sections[shstrndx]
    strings = data[names[4]:names[4] + names[5]]
    for section in sections:
        name = strings[section[0]:].split(b"\0", 1)[0]
        if name == b".text":
            return data[section[4]:section[4] + section[5]]
    raise AssertionError("missing .text")


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--vmm", default="build/sapphire-vmm")
    args = parser.parse_args()
    base = Path(__file__).resolve().parent.parent
    with tempfile.TemporaryDirectory(prefix="smoke-", dir=base / "build") as tmp:
        tmp = Path(tmp)
        subprocess.run([
            "xcrun", "clang", "--target=aarch64-none-elf", "-c",
            str(base / "tests/smoke.S"), "-o", str(tmp / "smoke.o")
        ], check=True)
        (tmp / "Image").write_bytes(text_section(tmp / "smoke.o"))
        (tmp / "initrd").write_bytes(b"\x1f\x8b")
        (tmp / "root").write_bytes(bytes(4096))
        (tmp / "data").write_bytes(bytes(4096))
        parent, child = socket.socketpair(socket.AF_UNIX, socket.SOCK_DGRAM)
        try:
            result = subprocess.run([
                str(base / args.vmm), "--kernel", str(tmp / "Image"),
                "--initrd", str(tmp / "initrd"),
                "--root-disk", str(tmp / "root"),
                "--data-disk", str(tmp / "data"),
                "--cpus", "2", "--memory-mib", "128",
                "--net-fd", str(child.fileno()),
                "--vsock", str(tmp / "vsock"), "--console", str(tmp / "console"),
            ], pass_fds=[child.fileno()], capture_output=True, timeout=15)
            print(result.stderr.decode(), end="")
            console = (tmp / "console").read_text()
            print("Synthetic guest console:", repr(console))
            assert result.returncode == 0, result.returncode
            assert sorted(console.splitlines()) == ["B", "P", "S"], console
            print("PASS: real HV execution, EL1 FDT x0, PL011, PSCI, two vCPUs")
            subprocess.run([
                "xcrun", "clang", "--target=aarch64-none-elf", "-c",
                str(base / "tests/idle.S"), "-o", str(tmp / "idle.o")
            ], check=True)
            (tmp / "Image").write_bytes(text_section(tmp / "idle.o"))
            (tmp / "console").unlink()
            parent_read, parent_write = os.pipe()
            child_vm = subprocess.Popen([
                str(base / args.vmm), "--kernel", str(tmp / "Image"),
                "--initrd", str(tmp / "initrd"),
                "--root-disk", str(tmp / "root"),
                "--data-disk", str(tmp / "data"),
                "--cpus", "2", "--memory-mib", "128",
                "--net-fd", str(child.fileno()),
                "--parent-fd", str(parent_read),
                "--vsock", str(tmp / "vsock"), "--console", str(tmp / "console"),
            ], pass_fds=[child.fileno(), parent_read], stderr=subprocess.PIPE)
            os.close(parent_read)
            try:
                deadline = time.monotonic() + 5
                while time.monotonic() < deadline:
                    if (tmp / "console").exists() and "I\n" in (tmp / "console").read_text():
                        break
                    assert child_vm.poll() is None, "idle guest failed to start"
                    time.sleep(0.02)
                else:
                    raise AssertionError("idle guest did not reach WFI")
                os.close(parent_write)
                parent_write = -1
                _, err = child_vm.communicate(timeout=5)
                print(err.decode(), end="")
                assert child_vm.returncode == 0
                assert b"parent lifetime pipe closed" in err
                print("PASS: parent EOF interrupts blocked native WFI and stops all threads")
            finally:
                if parent_write >= 0:
                    os.close(parent_write)
                if child_vm.poll() is None:
                    child_vm.kill()
                    child_vm.communicate()
            subprocess.run([
                "xcrun", "clang", "--target=aarch64-none-elf", "-c",
                str(base / "tests/reset.S"), "-o", str(tmp / "reset.o")
            ], check=True)
            (tmp / "Image").write_bytes(text_section(tmp / "reset.o"))
            (tmp / "console").unlink()
            parent_read, parent_write = os.pipe()
            try:
                began = time.monotonic()
                result = subprocess.run([
                    str(base / args.vmm), "--kernel", str(tmp / "Image"),
                    "--initrd", str(tmp / "initrd"),
                    "--root-disk", str(tmp / "root"),
                    "--data-disk", str(tmp / "data"),
                    "--cpus", "2", "--memory-mib", "128",
                    "--net-fd", str(child.fileno()),
                    "--parent-fd", str(parent_read),
                    "--vsock", str(tmp / "vsock"), "--console", str(tmp / "console"),
                ], pass_fds=[child.fileno(), parent_read],
                   capture_output=True, timeout=10)
                print(result.stderr.decode(), end="")
                assert result.returncode == 1, result.returncode
                assert time.monotonic() - began < 10
                assert (tmp / "console").read_text().splitlines() == ["R"] * 3
                assert b"rapid reset limit reached" in result.stderr
                assert result.stderr.count(b"running 2 vCPUs") == 3
                assert b"network fd" not in result.stderr
                assert not (tmp / "vsock").exists()
                assert parent.getpeername() is not None
                print("PASS: two resets preserve network/parent FDs; third rapid reset "
                      "fails cleanly without a reboot storm")
            finally:
                os.close(parent_read)
                os.close(parent_write)
        finally:
            parent.close()
            child.close()


if __name__ == "__main__":
    main()

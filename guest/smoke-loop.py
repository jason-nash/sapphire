#!/usr/bin/env python3
"""Check machine loop direct I/O in a new, disposable Sapphire VM."""

import argparse
import hashlib
import json
import os
from pathlib import Path
import shutil
import signal
import subprocess
import sys
import tempfile


def main():
    project = Path(__file__).resolve().parent.parent
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--disposable-guest", action="store_true", required=True)
    parser.add_argument("--engine", type=Path, default=project / "engine/build/sapphire-engine")
    parser.add_argument("--vmm", type=Path, default=project / "native/build/sapphire-vmm")
    parser.add_argument("--assets", type=Path, default=project / "guest/assets")
    parser.add_argument("--keep", action="store_true")
    args = parser.parse_args()
    for tool in (args.engine, args.vmm):
        if not tool.is_file() or not os.access(tool, os.X_OK):
            parser.error(f"missing executable: {tool}")
    for name in ("kernel", "initrd.gz", "rootfs.img"):
        if not (args.assets / name).is_file():
            parser.error(f"missing asset: {args.assets / name}")

    home = Path(tempfile.mkdtemp(prefix="sgdio-", dir="/tmp"))
    environment = dict(os.environ, SAPPHIRE_HOME=str(home), SAPPHIRE_VMM=str(args.vmm.resolve()))
    started, passed = False, False

    def interrupted(signum, _frame):
        raise KeyboardInterrupt(f"signal {signum}")

    signal.signal(signal.SIGTERM, interrupted)

    def engine(*arguments, timeout=330):
        result = subprocess.run(
            [str(args.engine.resolve()), *arguments], env=environment,
            text=True, capture_output=True, timeout=timeout,
        )
        if result.returncode:
            raise RuntimeError(f"engine {arguments[0]}: {(result.stderr + result.stdout)[-4000:]}")
        return result.stdout

    def call(action, **fields):
        response = json.loads(engine("call", json.dumps(dict(action=action, **fields))))
        if not response["ok"]:
            raise RuntimeError(f"{action}: {response['error']}")
        return response["result"]

    def execute(name, command):
        result = call("machine.exec", name=name, arguments=["/bin/sh", "-ec", command])
        if result["exitCode"] or result["truncated"]:
            raise RuntimeError(f"machine exec: {result}")
        return result["stdout"].strip()

    def verify(name):
        result = execute(name, """
device=$(awk '$5 == "/" { print $3; exit }' /proc/self/mountinfo)
printf '%s\\n' "$device"
cat "/sys/dev/block/$device/loop/dio"
cat "/sys/dev/block/$device/loop/autoclear"
cat "/sys/dev/block/$device/queue/logical_block_size"
cat "/sys/dev/block/$device/loop/backing_file"
count=0
for file in /sys/block/loop*/loop/backing_file; do
    test -e "$file" || continue
    count=$((count + 1))
done
printf '%s\\n' "$count"
""").splitlines()
        if len(result) != 6 or result[1:4] != ["1", "1", "4096"] or result[5] != "1":
            raise RuntimeError(f"unverified direct I/O or leaked loop attachment: {result}")
        if result[4] != f"/var/lib/sapphire/machines/{name}/disk.img":
            raise RuntimeError(f"wrong backing image: {result[4]}")
        return dict(device=result[0], directIO=True, autoclear=True, sectorSize=4096, backing=result[4])

    try:
        (home / "assets").mkdir(mode=0o700)
        hashes = {}
        for name in ("kernel", "initrd.gz", "rootfs.img"):
            source, destination = args.assets / name, home / "assets" / name
            hashes[name] = hashlib.sha256(source.read_bytes()).hexdigest()
            shutil.copyfile(source, destination)
            if hashlib.sha256(destination.read_bytes()).hexdigest() != hashes[name]:
                raise RuntimeError(f"asset changed during copy: {name}")
        started = True
        engine("start", "--cpus", "2", "--memory-mib", "2048", "--data-gib", "16", "--json", timeout=130)
        call("machine.create", configuration=dict(
            name="directio", distribution="alpine", cpus=1, memoryGiB=1, diskGiB=8,
            docker=False, rosetta=False, shareHome=False,
        ))
        call("machine.start", name="directio")
        first = verify("directio")
        checksum = execute("directio", """
dd if=/dev/urandom of=/root/directio.bin bs=1048576 count=16
sync
sha256sum /root/directio.bin
""")
        call("machine.stop", name="directio")
        call("machine.start", name="directio")
        restarted = verify("directio")
        if execute("directio", "sha256sum /root/directio.bin") != checksum:
            raise RuntimeError("write did not survive direct-I/O disk remount")
        call("machine.stop", name="directio")
        call("machine.clone", name="directio", destination="directio-clone")
        call("machine.start", name="directio-clone")
        cloned = verify("directio-clone")
        if execute("directio-clone", "sha256sum /root/directio.bin") != checksum:
            raise RuntimeError("clone data differs")
        call("machine.stop", name="directio-clone")
        call("machine.start", name="directio")
        verify("directio")
        call("machine.delete", name="directio-clone")
        call("machine.stop", name="directio")
        call("machine.delete", name="directio")
        print(json.dumps(dict(
            result="passed", assets=hashes, initial=first, restarted=restarted, clone=cloned,
            checks=["direct I/O effective", "autoclear effective", "one attached loop",
                    "16 MiB write/read", "remount persistence", "clone persistence", "stop/delete"],
        ), indent=2), flush=True)
        passed = True
    finally:
        if started:
            try:
                engine("stop", "--json", timeout=65)
            except Exception as error:
                passed = False
                print(f"cleanup of owned VM failed: {error}", file=sys.stderr)
        if not passed:
            console = home / "console.log"
            if console.exists():
                print("\n".join(console.read_text(errors="replace").splitlines()[-100:]), file=sys.stderr)
        if passed and not args.keep:
            shutil.rmtree(home)
        else:
            print(f"Test files retained at {home}", file=sys.stderr)
        if not passed and sys.exc_info()[0] is None:
            raise RuntimeError("test VM cleanup failed")


if __name__ == "__main__":
    main()

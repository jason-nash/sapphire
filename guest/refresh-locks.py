#!/usr/bin/env python3
"""Explicit maintainer operation: resolve and hash official Alpine APK inputs."""

import concurrent.futures
import hashlib
import io
from pathlib import Path
import re
import tarfile
import urllib.request


BASE = "https://dl-cdn.alpinelinux.org/alpine/v3.23/"
PREFERRED = {
    "/bin/sh": "busybox-binsh",
    "libc-dev": "musl-dev",
    "pkgconfig": "pkgconf",
    "so:libisl.so.23": "isl26",
}
ROOTS = {
    "runtime": [
        "alpine-baselayout", "alpine-keys", "apk-tools", "busybox",
        "busybox-binsh", "ca-certificates", "docker-engine", "docker-cli",
        "iptables", "iproute2-minimal", "e2fsprogs", "e2fsprogs-extra",
        "mount", "umount", "losetup", "util-linux-misc", "coreutils",
    ],
    "build": [
        "build-base", "bash", "bc", "bison", "flex", "openssl-dev", "elfutils-dev",
        "perl", "python3", "kmod", "lddtree", "cpio", "squashfs-tools",
        "e2fsprogs", "blkid", "busybox-static", "findutils", "curl",
        "xz", "gzip", "coreutils",
    ],
}


def main():
    records = {}
    providers = {}
    for repository in ("main", "community"):
        with urllib.request.urlopen(BASE + repository + "/aarch64/APKINDEX.tar.gz") as response:
            data = response.read()
        with tarfile.open(fileobj=io.BytesIO(data), mode="r:gz") as archive:
            text = archive.extractfile("APKINDEX").read().decode()
        for entry in text.split("\n\n"):
            record = dict(line.split(":", 1) for line in entry.splitlines() if ":" in line)
            if "P" in record:
                record["repository"] = repository
                records[record["P"]] = record
    for name, record in records.items():
        for item in record.get("p", "").split():
            alias = re.split("[=<>~]", item, maxsplit=1)[0]
            providers.setdefault(alias, []).append(name)

    def closure(names):
        selected = {}
        pending = list(names)
        while pending:
            dependency = pending.pop()
            if dependency.startswith("!"):
                continue
            dependency = re.split("[=<>~]", dependency, maxsplit=1)[0]
            if dependency in records:
                name = dependency
            elif dependency in PREFERRED:
                name = PREFERRED[dependency]
            else:
                options = providers.get(dependency, [])
                if len(options) != 1:
                    raise ValueError(f"Choose an explicit provider for {dependency}: {options}")
                name = options[0]
            if name in selected:
                continue
            selected[name] = records[name]
            pending.extend(records[name].get("D", "").split())
        return selected

    groups = {kind: closure(roots) for kind, roots in ROOTS.items()}
    packages = {name: record for group in groups.values() for name, record in group.items()}
    print(f"Hashing {len(packages)} official APKs ({sum(int(r['S']) for r in packages.values())} bytes)", flush=True)

    def checksum(item):
        name, record = item
        url = BASE + record["repository"] + "/aarch64/" + name + "-" + record["V"] + ".apk"
        digest = hashlib.sha256()
        with urllib.request.urlopen(url) as response:
            while data := response.read(1 << 20):
                digest.update(data)
        return name, (digest.hexdigest(), url)

    with concurrent.futures.ThreadPoolExecutor(max_workers=8) as pool:
        hashes = dict(pool.map(checksum, packages.items()))
    for kind, packages in groups.items():
        lines = ["# SHA256 and official Alpine 3.23/aarch64 URL; transitive dependencies included."]
        lines += [" ".join(hashes[name]) for name in sorted(packages)]
        (Path(__file__).parent / (kind + "-apks.lock")).write_text("\n".join(lines) + "\n")
        print(f"{kind}: {len(packages)} pinned APKs", flush=True)


if __name__ == "__main__":
    main()

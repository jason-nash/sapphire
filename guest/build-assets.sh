#!/bin/sh
set -eu
export LC_ALL=C TZ=UTC
export SOURCE_DATE_EPOCH=${SOURCE_DATE_EPOCH:-1792540800}

out=/out
initroot=/build/initramfs
root=/build/rootfs
mkdir -p "$out" "$initroot/bin" "$initroot/sbin" "$initroot/etc" "$initroot/dev" "$initroot/proc" "$initroot/sys"

release=$(cat /build/kernel-release)
grep -qx 'CONFIG_RTC_HCTOSYS_DEVICE="rtc0"' "$out/kernel.config" \
    || { echo "Kernel must initialize the wall clock from rtc0" >&2; exit 1; }

# Dependency closure only; no kernel modules or build tools in the rootfs.
module_directory="lib/modules/$release"
mkdir -p "$initroot/$module_directory"
cp "/build/modules/$module_directory/modules.builtin" "$initroot/$module_directory/"
cp "/build/modules/$module_directory/modules.builtin.modinfo" "$initroot/$module_directory/"
cp "/build/modules/$module_directory/modules.order" "$initroot/$module_directory/"
depmod -b /build/modules "$release"
for module in virtio_mmio virtio_blk virtio_net vmw_vsock_virtio_transport \
    virtio_balloon virtio_console virtio_rng loop squashfs overlay ext4; do
    modprobe -d /build/modules -S "$release" --show-depends "$module" > /build/dependencies
    while read -r operation filename rest; do
        [ "$operation" = insmod ] || continue
        relative=${filename#/build/modules/}
        mkdir -p "$initroot/$(dirname "$relative")"
        cp "$filename" "$initroot/$relative"
    done </build/dependencies
done
depmod -b "$initroot" "$release"

cp /bin/busybox.static "$initroot/bin/busybox"
# A static shell plus explicit dynamic tools and their entire ELF dependency tree.
"$initroot/bin/busybox" --list | while IFS= read -r applet; do
    [ "$applet" = busybox ] || ln -s /bin/busybox "$initroot/bin/$applet"
done
copy_binary() {
    binary=$(command -v "$1")
    mkdir -p "$initroot$(dirname "$binary")"
    cp -L "$binary" "$initroot$binary"
    for file in $(lddtree -l "$binary"); do
        mkdir -p "$initroot$(dirname "$file")"
        cp -L "$file" "$initroot$file"
    done
}
copy_binary modprobe
copy_binary blkid
copy_binary mkfs.ext4
cp /src/initramfs/init "$initroot/init"
cp /build/sapphire-guest "$initroot/sbin/sapphire-guest"
chmod 0755 "$initroot/init" "$initroot/sbin/sapphire-guest"
# Busybox's applets in /bin must not shadow the dependency-aware kmod tools.
rm -f "$initroot/bin/modprobe" "$initroot/bin/blkid" "$initroot/bin/mkfs.ext4"
chroot "$initroot" /bin/sh -ec \
    'export PATH=/bin:/sbin:/usr/bin:/usr/sbin
     for applet in mkdir mount sleep switch_root cat rmdir; do
         test -x "/bin/$applet" || exit 1
     done
     mkdir -p /run/applet-check
     rmdir /run/applet-check
     echo "PASS initramfs mkdir and required BusyBox applets"
     modprobe --version; blkid --version; mkfs.ext4 -V
     /sbin/sapphire-guest --version'

mkdir -p "$root"
apk --root "$root" --initdb --no-network --keys-dir /etc/apk/keys add /build/runtime-apks/*.apk
mkdir -p "$root"/dev "$root"/proc "$root"/sys "$root"/run "$root"/tmp \
    "$root"/mnt/sapphire/base "$root"/mnt/sapphire/data \
    "$root"/var/lib/sapphire/machines "$root"/usr/share/sapphire/rootfs \
    "$root"/etc/docker "$root"/etc/apk "$root"/usr/local/bin
chmod 1777 "$root/tmp"
cp /build/sapphire-guest "$root/sbin/sapphire-guest"
cp /src/udhcpc.script "$root/usr/share/sapphire/udhcpc.script"
chmod 0755 "$root/usr/share/sapphire/udhcpc.script" "$root/sbin/sapphire-guest"
cp /build/alpine-minirootfs.tar.gz "$root/usr/share/sapphire/rootfs/alpine.tar.gz"
printf '%s  alpine.tar.gz\n' "$ALPINE_ROOTFS_SHA256" > "$root/usr/share/sapphire/rootfs/alpine.tar.gz.sha256"
cp -a /etc/apk/keys "$root/etc/apk/keys"
printf '%s\n' \
    https://dl-cdn.alpinelinux.org/alpine/v3.23/main \
    https://dl-cdn.alpinelinux.org/alpine/v3.23/community > "$root/etc/apk/repositories"
printf '%s\n' '{"features":{"containerd-snapshotter":false},"storage-driver":"overlay2"}' > "$root/etc/docker/daemon.json"
printf 'nameserver 1.1.1.1\n' > "$root/etc/resolv.conf"
printf 'sapphire\n' > "$root/etc/hostname"
printf '127.0.0.1 localhost sapphire\n::1 localhost\n' > "$root/etc/hosts"
printf '%s\n' "$release" > "$root/usr/share/sapphire/kernel-release"
cp /src/runtime-apks.lock "$root/usr/share/sapphire/runtime-apks.lock"
chroot "$root" /bin/sh -ec 'losetup --version; losetup --help | grep -F -- --direct-io'

if [ "${INCLUDE_K3S:-0}" = 1 ]; then
    [ "${#K3S_SHA256}" -eq 64 ] || { echo "Exact K3S_SHA256 is required" >&2; exit 1; }
    case "$K3S_VERSION" in v[0-9]*+k3s[0-9]*) ;; *) echo "Invalid pinned K3S_VERSION" >&2; exit 1;; esac
    curl -fL --retry 3 "https://github.com/k3s-io/k3s/releases/download/$K3S_VERSION/k3s-arm64" -o "$root/usr/local/bin/k3s"
    printf '%s  %s\n' "$K3S_SHA256" "$root/usr/local/bin/k3s" | sha256sum -c -
    chmod 0755 "$root/usr/local/bin/k3s"
    printf '{"version":"%s","sha256":"%s"}\n' "$K3S_VERSION" "$K3S_SHA256" > "$root/usr/share/sapphire/k3s.json"
fi

# Package caches and toolchains stay outside the immutable guest filesystem.
rm -rf "$root/var/cache/apk" "$root/lib/modules" "$root/usr/share/man" "$root/usr/share/doc"
# apk records the wall-clock install time inside this build-only log.
rm -f "$root/var/log/apk.log"
find "$root" "$initroot" -exec touch -h -d "@$SOURCE_DATE_EPOCH" {} +
env -u SOURCE_DATE_EPOCH mksquashfs "$root" "$out/rootfs.img" -noappend -comp zstd -processors 1 \
    -all-root -all-time "$SOURCE_DATE_EPOCH" -mkfs-time "$SOURCE_DATE_EPOCH" -no-progress
cd "$initroot"
find . -print0 | sort -z | cpio --null -o -H newc --reproducible --owner=0:0 > /build/files.cpio
# gen_init_cpio encodes device nodes without a privileged build or mknod.
printf '%s\n' \
    'dir /dev 755 0 0' \
    'nod /dev/console 600 0 0 c 5 1' \
    'nod /dev/null 666 0 0 c 1 3' > /build/devices.list
/build/gen_init_cpio -t "$SOURCE_DATE_EPOCH" /build/devices.list > /build/devices.cpio
cat /build/files.cpio /build/devices.cpio | gzip -n -9 > "$out/initrd.gz"
cd "$out"
sha256sum kernel initrd.gz rootfs.img > SHA256SUMS

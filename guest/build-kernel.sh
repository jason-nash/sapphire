#!/bin/sh
set -eu
export LC_ALL=C TZ=UTC
export SOURCE_DATE_EPOCH=${SOURCE_DATE_EPOCH:-1792540800}
export KBUILD_BUILD_USER=sapphire KBUILD_BUILD_HOST=builder KBUILD_BUILD_VERSION=1
export KBUILD_BUILD_TIMESTAMP="$(date -u -d "@$SOURCE_DATE_EPOCH" '+%a %b %d %T UTC %Y')"
mkdir -p /out
cd /build/linux
make ARCH=arm64 defconfig
# Generic virtual hardware only, not dozens of unrelated phone/SoC platforms.
for symbol in $(sed -n 's/^config \(ARCH_[A-Z0-9_]*\)$/\1/p' arch/arm64/Kconfig.platforms); do
    [ "$symbol" = ARCH_VIRT ] || scripts/config --disable "$symbol"
done
for symbol in $(sed -n 's/^\(CONFIG_NET_VENDOR_[A-Z0-9_]*\)=y$/\1/p' .config); do
    scripts/config --disable "${symbol#CONFIG_}"
done
scripts/kconfig/merge_config.sh -m .config /src/kernel.config
make ARCH=arm64 olddefconfig
missing=0
while IFS= read -r setting; do
    case "$setting" in CONFIG_*=*)
        if ! grep -qxF "$setting" .config; then
            echo "Unsatisfied kernel option: $setting" >&2
            missing=1
        fi;;
    esac
done </src/kernel.config
[ "$missing" -eq 0 ] || exit 1
make -j"${BUILD_JOBS:-4}" ARCH=arm64 Image modules
make ARCH=arm64 INSTALL_MOD_PATH=/build/modules INSTALL_MOD_STRIP=1 modules_install
make -s ARCH=arm64 kernelrelease > /build/kernel-release
cp arch/arm64/boot/Image /out/kernel
cp .config /out/kernel.config

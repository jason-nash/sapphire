#!/bin/bash
set -euo pipefail

project_root="$(cd "$(dirname "$0")/.." && pwd)"
prefix="${SAPPHIRE_PREFIX:-$HOME/.local}"
runtime_home="${SAPPHIRE_HOME:-$HOME/.sapphire}"
if [[ $# != 0 ]]; then
  echo "Usage: SAPPHIRE_PREFIX=~/.local SAPPHIRE_HOME=~/.sapphire scripts/install.sh" >&2
  exit 2
fi
for executable in sapphire sapphire-engine sapphire-vmm; do
  if [[ ! -x "$project_root/dist/bin/$executable" ]]; then
    echo "Run scripts/build.sh before installation." >&2
    exit 1
  fi
done
if [[ -S "$runtime_home/control.sock" ]]; then
  echo "Stop the Sapphire runtime before replacing its installation." >&2
  exit 1
fi
install -d -m 755 "$prefix/bin"
for executable in sapphire sapphire-engine sapphire-vmm; do
  install -m 755 "$project_root/dist/bin/$executable" "$prefix/bin/$executable"
done
if [[ -d "$project_root/guest/assets" ]]; then
  install -d -m 700 "$runtime_home" "$runtime_home/assets"
  for asset in kernel initrd.gz rootfs.img; do
    install -m 600 "$project_root/guest/assets/$asset" "$runtime_home/assets/$asset"
  done
fi
echo "Installed Sapphire tools in $prefix/bin"
echo "Guest assets belong in $runtime_home/assets; run sapphire doctor to check them."

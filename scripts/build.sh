#!/bin/bash
set -euo pipefail

project_root="$(cd "$(dirname "$0")/.." && pwd)"
cd "$project_root"
with_guest=false
if [[ "${1:-}" == "--with-guest" && $# == 1 ]]; then
  with_guest=true
elif [[ $# != 0 ]]; then
  echo "Usage: scripts/build.sh [--with-guest]" >&2
  exit 2
fi
if [[ "$(uname -s)" != Darwin || "$(uname -m)" != arm64 ]]; then
  echo "Sapphire's host runtime requires Apple Silicon and macOS 15 or later." >&2
  exit 1
fi

make -C native all
mkdir -p engine/build dist/bin
(
  cd engine
  go build -trimpath -o build/sapphire-engine ./cmd/sapphire-engine
)
swift build -c release
swift_bin="$(swift build -c release --show-bin-path)"
install -m 755 "$swift_bin/sapphire" dist/bin/sapphire
install -m 755 engine/build/sapphire-engine dist/bin/sapphire-engine
install -m 755 native/build/sapphire-vmm dist/bin/sapphire-vmm
if $with_guest; then
  guest/build.sh --with-k3s
fi
scripts/build-app.sh
echo "Host tools: $project_root/dist/bin"
echo "Application: $project_root/dist/Sapphire.app"

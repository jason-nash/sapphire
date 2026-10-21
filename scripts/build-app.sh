#!/bin/bash
set -euo pipefail

project_root="$(cd "$(dirname "$0")/.." && pwd)"
cd "$project_root"

if [[ "$(uname -s)" != Darwin ]]; then
  echo "Building Sapphire.app requires macOS." >&2
  exit 1
fi
if [[ $# != 0 ]]; then
  echo "Usage: scripts/build-app.sh" >&2
  exit 2
fi

swift build -c release --product SapphireDesktop
swift build -c release --product sapphire
app_bin="$(swift build -c release --show-bin-path)"
cli_bin="$app_bin"

mkdir -p dist/bin
install -m 755 "$cli_bin/sapphire" dist/bin/sapphire

engine_binary="${SAPPHIRE_ENGINE_BINARY:-$project_root/dist/bin/sapphire-engine}"
vmm_binary="${SAPPHIRE_VMM_BINARY:-$project_root/dist/bin/sapphire-vmm}"
if [[ ! -x "$engine_binary" || ! -x "$vmm_binary" ]]; then
  echo "Missing sapphire-engine or sapphire-vmm. Run scripts/build.sh first, or set SAPPHIRE_ENGINE_BINARY and SAPPHIRE_VMM_BINARY." >&2
  exit 1
fi

staging="$(mktemp -d "$project_root/dist/.sapphire-app.XXXXXX")"
trap 'rm -rf "$staging"' EXIT
bundle="$staging/Sapphire.app"
mkdir -p "$bundle/Contents/MacOS" "$bundle/Contents/Resources"
install -m 644 packaging/Info.plist "$bundle/Contents/Info.plist"
install -m 755 "$app_bin/SapphireDesktop" "$bundle/Contents/MacOS/SapphireDesktop"
install -m 755 "$cli_bin/sapphire" "$bundle/Contents/MacOS/sapphire"
install -m 755 "$engine_binary" "$bundle/Contents/MacOS/sapphire-engine"
install -m 755 "$vmm_binary" "$bundle/Contents/MacOS/sapphire-vmm"
if [[ ! "$engine_binary" -ef dist/bin/sapphire-engine ]]; then
  install -m 755 "$engine_binary" dist/bin/sapphire-engine
fi
if [[ ! "$vmm_binary" -ef dist/bin/sapphire-vmm ]]; then
  install -m 755 "$vmm_binary" dist/bin/sapphire-vmm
fi

# Offscreen SwiftUI drawing creates the icon without opening an application window.
iconset="$staging/Sapphire.iconset"
mkdir -p "$iconset"
"$app_bin/SapphireDesktop" --render-icon "$staging/icon.png"
for size in 16 32 128 256 512; do
  sips -z "$size" "$size" "$staging/icon.png" --out "$iconset/icon_${size}x${size}.png" >/dev/null
  retina_size=$((size * 2))
  sips -z "$retina_size" "$retina_size" "$staging/icon.png" --out "$iconset/icon_${size}x${size}@2x.png" >/dev/null
done
iconutil -c icns "$iconset" -o "$bundle/Contents/Resources/Sapphire.icns"

# Preserve the VMM's Hypervisor entitlement when applying an ad hoc signature.
codesign -d --entitlements - --xml "$vmm_binary" > "$staging/vmm-entitlements.plist" 2>/dev/null
if [[ "$(/usr/libexec/PlistBuddy -c 'Print :com.apple.security.hypervisor' "$staging/vmm-entitlements.plist")" != true ]]; then
  echo "The VMM must already carry its com.apple.security.hypervisor entitlement. Build it with native/Makefile." >&2
  exit 1
fi
codesign --force --sign - --entitlements "$staging/vmm-entitlements.plist" "$bundle/Contents/MacOS/sapphire-vmm"
codesign --force --sign - "$bundle/Contents/MacOS/sapphire-engine"
codesign --force --sign - "$bundle/Contents/MacOS/sapphire"
codesign --force --sign - "$bundle"
codesign --verify --deep --strict "$bundle"
plutil -lint "$bundle/Contents/Info.plist"

if [[ -e dist/Sapphire.app ]]; then
  rm -rf dist/Sapphire.app
fi
mv "$bundle" dist/Sapphire.app
echo "Built $project_root/dist/Sapphire.app"
echo "CLI: $project_root/dist/bin/sapphire"

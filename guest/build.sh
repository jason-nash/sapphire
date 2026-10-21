#!/bin/sh
set -eu

guest_directory=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
output=${SAPPHIRE_GUEST_OUTPUT:-"$guest_directory/assets"}
jobs=${BUILD_JOBS:-4}
include_k3s=${INCLUDE_K3S:-0}
while [ "$#" -gt 0 ]; do
    case "$1" in
        --output)
            [ "$#" -ge 2 ] || { echo "--output requires a directory" >&2; exit 2; }
            output=$2
            shift 2
            ;;
        --jobs)
            [ "$#" -ge 2 ] || { echo "--jobs requires a positive integer" >&2; exit 2; }
            jobs=$2
            shift 2
            ;;
        --with-k3s) include_k3s=1; shift;;
        --help|-h)
            echo "Usage: guest/build.sh [--output DIRECTORY] [--jobs N] [--with-k3s]"
            echo "Exports kernel, initrd.gz, rootfs.img, kernel.config, and SHA256SUMS."
            exit 0
            ;;
        *) echo "Unknown argument: $1" >&2; exit 2;;
    esac
done
case "$jobs" in ''|*[!0-9]*|0) echo "--jobs must be a positive integer" >&2; exit 2;; esac
case "$include_k3s" in 0|1) ;; *) echo "INCLUDE_K3S must be 0 or 1" >&2; exit 2;; esac
mkdir -p "$output"
output=$(CDPATH= cd -- "$output" && pwd)
exec docker buildx build --platform linux/arm64 \
    --file "$guest_directory/Dockerfile" \
    --build-arg "BUILD_JOBS=$jobs" \
    --build-arg "INCLUDE_K3S=$include_k3s" \
    --output "type=local,dest=$output" "$guest_directory"

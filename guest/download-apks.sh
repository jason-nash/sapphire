#!/bin/sh
set -eu
lock=$1
destination=$2
mkdir -p "$destination"
while read -r checksum url; do
    case "$checksum" in ''|\#*) continue;; esac
    file="$destination/${url##*/}"
    wget -q -O "$file" "$url"
    printf '%s  %s\n' "$checksum" "$file" | sha256sum -c -
done <"$lock"

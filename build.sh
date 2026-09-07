#!/bin/sh
set -eu

script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
cd "$script_dir"
if ! pkg-config --atleast-version=0.3 vterm; then
    echo "Install libvterm development headers and pkg-config (Arch: libvterm pkgconf; Debian/Ubuntu: libvterm-dev pkg-config)." >&2
    exit 1
fi
go test ./...
go build -buildvcs=false -o kiwikube ./src
echo "Built $script_dir/kiwikube"

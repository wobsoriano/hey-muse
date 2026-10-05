#!/usr/bin/env bash
# build-pico.sh — compile tools/pico (SVOX Pico and techo5-pico.c, the device's own voice) for armv7
# musl, statically, with zig's C compiler. zig carries musl and the cross linker itself, so this
# needs no Alpine root, no QEMU and no WSL, and runs the same on macOS, Linux and WSL.
#
#   tools/linux/build-pico.sh [-o bin/techo5-pico-arm]
#
# Needs zig on PATH (or ZIG): brew install zig, or https://ziglang.org/download/. Written with and
# checked against zig 0.17.0.
#
# -Os because the engine is light work for this CPU and a small helper starts quickly; -w because
# Pico's sources are from 2009 and warn in every file under a current compiler; -s strips. Hard float
# (musleabihf) is what Alpine's armv7 is, though a static binary would run either way.
set -euo pipefail
ROOT=${TECHO5_ROOT:-$(cd "$(dirname "$0")/../.." && pwd)}
ZIG=${ZIG:-zig}
OUT=$ROOT/bin/techo5-pico-arm
while [ $# -gt 0 ]; do
	case "$1" in
	-o) OUT=$2; shift 2;;
	*) echo "unknown argument: $1" >&2; exit 1;;
	esac
done

command -v "$ZIG" >/dev/null || { echo "zig is not installed: brew install zig, or https://ziglang.org/download/" >&2; exit 1; }
echo "== compiling with zig $("$ZIG" version)"
mkdir -p "$(dirname "$OUT")"
"$ZIG" cc -target arm-linux-musleabihf -mcpu=cortex_a53 -Os -static -s -w -std=gnu99 \
	-I "$ROOT/tools/pico/lib" -o "$OUT" "$ROOT/tools/pico/techo5-pico.c" "$ROOT"/tools/pico/lib/*.c -lm
ls -l "$OUT"
echo "built: $OUT"

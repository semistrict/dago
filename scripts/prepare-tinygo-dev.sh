#!/bin/sh
set -eu

# TinyGo 0.41.1 predates two js/wasm fixes needed with the current Go 1.26
# toolchain. Backport the exact changes from these pinned development commits
# until a stable TinyGo release contains them:
#
# - TinyGo 8793dc37facf72955dddada5e357a58bb6947b83, pointing at
#   tinygo-org/net 1026408a386a88504e6c958e31291dea058e41c0.
# - TinyGo 6233915ecf63017f3fe7dfbeaa9fcd86fddc3243.

script_dir=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
tinygo_bin=${TINYGO:-tinygo}
version=$($tinygo_bin version)
case "$version" in
	"tinygo version 0.41.1 "*) ;;
	*)
		echo "unsupported TinyGo version: $version" >&2
		exit 1
		;;
esac

if command -v sha256sum >/dev/null 2>&1; then
	checksum() { sha256sum "$1" | awk '{print $1}'; }
else
	checksum() { shasum -a 256 "$1" | awk '{print $1}'; }
fi

patch_checked() {
	label=$1
	target=$2
	stable_sha=$3
	fixed_sha=$4
	patch_file=$5
	actual=$(checksum "$target")
	if test "$actual" = "$fixed_sha"; then
		return
	fi
	if test "$actual" != "$stable_sha"; then
		echo "unexpected $label checksum: $actual" >&2
		exit 1
	fi
	patch -s "$target" < "$patch_file"
	actual=$(checksum "$target")
	if test "$actual" != "$fixed_sha"; then
		echo "patched $label checksum mismatch: $actual" >&2
		exit 1
	fi
}

tinygo_root=$($tinygo_bin env TINYGOROOT)
patch_checked \
	"TinyGo net/http source" \
	"$tinygo_root/src/net/http/roundtrip_js.go" \
	"1e2145bab2ab569949f88086ea4bcaa52f4a934e37fcd7a130d61b27f2fbed79" \
	"275c486b9b8153ec831f125ff6be4665cc1d401686b80ea77e0c4c94b9a5661f" \
	"$script_dir/tinygo-net-http-js.patch"
patch_checked \
	"TinyGo WebAssembly runtime" \
	"$tinygo_root/targets/wasm_exec.js" \
	"5994ca6b7da5cca65668ae88db354b240758499856a90379008dde56d702037d" \
	"4d2f44a002aed5ffbc77383729c4f012641e2518b0d7f326b2b5327abd312836" \
	"$script_dir/tinygo-wasm-exec-random.patch"

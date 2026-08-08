#!/bin/sh
set -eu

manifest="${1:-docs/shelley-upstream-tests.sha256}"
root="${2:-examples/shelley}"
failed=0

while IFS='  ' read -r expected path; do
	case "$expected" in
		'' | \#*) continue ;;
	esac
	if test ! -f "$root/$path"; then
		printf 'missing upstream Shelley test artifact: %s\n' "$path" >&2
		failed=1
		continue
	fi
	actual="$(shasum -a 256 "$root/$path" | awk '{print $1}')"
	if test "$actual" != "$expected"; then
		printf 'modified upstream Shelley test artifact: %s\n' "$path" >&2
		failed=1
	fi
done <"$manifest"

exit "$failed"

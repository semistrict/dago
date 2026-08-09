#!/bin/sh
set -eu

manifest="${1:-docs/shelley-upstream-test-contracts.tsv}"
root="${2:-examples/shelley}"
failed=0

while IFS="$(printf '\t')" read -r kind path contract; do
	case "$kind" in
		'' | \#*) continue ;;
	esac
	if test ! -f "$root/$path"; then
		printf 'missing migrated Shelley test artifact: %s\n' "$path" >&2
		failed=1
		continue
	fi
	case "$kind" in
		artifact)
			;;
		go_case)
			if ! awk -v target="$contract" '
				$0 ~ ("^func " target "\\(") { found = 1 }
				END { exit !found }
			' "$root/$path"; then
				printf 'missing migrated Shelley Go test case: %s:%s\n' "$path" "$contract" >&2
				failed=1
			fi
			;;
		js_case_count)
			actual="$(awk '
				{
					line = $0
					while (match(line, /(describe|it|test)(\.[A-Za-z]+)?[[:space:]]*\(/)) {
						count++
						line = substr(line, RSTART + RLENGTH)
					}
				}
				END { print count + 0 }
			' "$root/$path")"
			if test "$actual" -lt "$contract"; then
				printf 'migrated Shelley JS test case count shrank: %s (want >= %s, got %s)\n' "$path" "$contract" "$actual" >&2
				failed=1
			fi
			;;
		*)
			printf 'unknown Shelley test contract kind: %s\n' "$kind" >&2
			failed=1
			;;
	esac
done <"$manifest"

exit "$failed"

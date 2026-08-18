#!/usr/bin/env bash
set -euo pipefail

positive_integer() {
  local value="$1"
  local name="$2"
  if ! [[ "$value" =~ ^[0-9]+$ ]] || [ "$((10#$value))" -eq 0 ]; then
    echo "::error::${name} must be a positive integer" >&2
    exit 1
  fi
}

boolean() {
  case "$1" in
    true|false) ;;
    *)
      echo "::error::$2 must be 'true' or 'false'" >&2
      exit 1
      ;;
  esac
}

if [ -z "${INPUT_PROMPT:-}" ]; then
  echo "::error::prompt must not be empty" >&2
  exit 1
fi
for required in DAGO_ACTION_BINARY INPUT_WORKING_DIRECTORY INPUT_STATE_DIR INPUT_SESSION_ID GITHUB_OUTPUT; do
  if [ -z "${!required:-}" ]; then
    echo "::error::${required} is required by the action runtime" >&2
    exit 1
  fi
done
if [ ! -x "$DAGO_ACTION_BINARY" ]; then
  echo "::error::the action binary is unavailable" >&2
  exit 1
fi

max_turns="${INPUT_MAX_TURNS:-50}"
timeout_seconds="${INPUT_TIMEOUT:-1800}"
quiet="${INPUT_QUIET:-true}"
positive_integer "$max_turns" max_turns
positive_integer "$timeout_seconds" timeout
boolean "$quiet" quiet

command=(
  "$DAGO_ACTION_BINARY"
  --cwd "$INPUT_WORKING_DIRECTORY"
  --state-dir "$INPUT_STATE_DIR"
  --resume "$INPUT_SESSION_ID"
  --stdin
  --max-turns "$max_turns"
  --timeout "$timeout_seconds"
  --shell-allow-list "${INPUT_SHELL_ALLOW_LIST:-recommended,git,gh}"
)
if [ -n "${INPUT_MODEL:-}" ]; then
  command+=(--model "$INPUT_MODEL")
fi
if [ -n "${INPUT_APPROVAL_MODEL:-}" ]; then
  command+=(--approval-model "$INPUT_APPROVAL_MODEL")
fi
if [ "$quiet" = true ]; then
  command+=(--quiet)
fi

stdout_file=$(mktemp "${RUNNER_TEMP:-${TMPDIR:-/tmp}}/dago-output.XXXXXX")
stderr_file=$(mktemp "${RUNNER_TEMP:-${TMPDIR:-/tmp}}/dago-error.XXXXXX")
child_pid=""
cleanup() {
  rm -f "$stdout_file" "$stderr_file"
}
trap cleanup EXIT

emit_outputs() {
  local status="$1"
  local delimiter
  delimiter="DAGO_$(od -An -N16 -tx1 /dev/urandom | tr -d ' \n')"
  if [ "$delimiter" = "DAGO_" ]; then
    echo "::error::unable to generate a safe output delimiter" >&2
    return 1
  fi
  {
    echo "exit_code=$status"
    echo "response<<$delimiter"
    cat "$stdout_file"
    echo
    echo "$delimiter"
  } >> "$GITHUB_OUTPUT"
}

cancel() {
  local status="$1"
  trap - INT TERM
  if [ -n "$child_pid" ]; then
    kill -TERM "$child_pid" 2>/dev/null || true
    wait "$child_pid" 2>/dev/null || true
  fi
  cat "$stdout_file"
  cat "$stderr_file" >&2
  emit_outputs "$status" || true
  exit "$status"
}
trap 'cancel 130' INT
trap 'cancel 143' TERM

set +e
"${command[@]}" >"$stdout_file" 2>"$stderr_file" < <(printf '%s' "$INPUT_PROMPT") &
child_pid=$!
wait "$child_pid"
status=$?
child_pid=""
set -e

cat "$stdout_file"
cat "$stderr_file" >&2
emit_outputs "$status"
exit "$status"

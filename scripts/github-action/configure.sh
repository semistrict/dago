#!/usr/bin/env bash
set -euo pipefail

require_env() {
  local name="$1"
  if [ -z "${!name:-}" ]; then
    echo "::error::${name} is required by the action runtime" >&2
    exit 1
  fi
}

hash_text() {
  if command -v sha256sum >/dev/null 2>&1; then
    printf '%s' "$1" | sha256sum | awk '{print substr($1, 1, 16)}'
  else
    printf '%s' "$1" | shasum -a 256 | awk '{print substr($1, 1, 16)}'
  fi
}

require_env GITHUB_WORKSPACE
require_env RUNNER_TEMP
require_env GITHUB_OUTPUT
require_env GITHUB_RUN_ID
require_env RUNNER_OS

case "${INPUT_ENABLE_MEMORY:-true}" in
  true|false) ;;
  *)
    echo "::error::enable_memory must be 'true' or 'false'" >&2
    exit 1
    ;;
esac

agent_name="${INPUT_AGENT_NAME:-agent}"
if ! [[ "$agent_name" =~ ^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$ ]]; then
  echo "::error::agent_name must contain 1-64 letters, digits, dots, underscores, or hyphens" >&2
  exit 1
fi

workspace=$(cd "$GITHUB_WORKSPACE" && pwd -P)
working_input="${INPUT_WORKING_DIRECTORY:-.}"
if [[ "$working_input" == *$'\n'* || "$working_input" == *$'\r'* ]]; then
  echo "::error::working_directory must not contain newlines" >&2
  exit 1
fi
if ! working_directory=$(cd "$workspace/$working_input" 2>/dev/null && pwd -P); then
  echo "::error::working_directory does not name an existing directory" >&2
  exit 1
fi
case "$working_directory/" in
  "$workspace/"*) ;;
  *)
    echo "::error::working_directory must remain inside the checked-out workspace" >&2
    exit 1
    ;;
esac

scope="${INPUT_MEMORY_SCOPE:-branch}"
case "$scope" in
  repo)
    scope_identity="repo"
    ;;
  branch)
    scope_identity="ref:${GITHUB_REF_NAME:-detached}"
    ;;
  pr)
    if [ -n "${GITHUB_EVENT_PR_NUMBER:-}" ]; then
      scope_identity="pr:${GITHUB_EVENT_PR_NUMBER}"
    else
      scope_identity="ref:${GITHUB_REF_NAME:-detached}"
    fi
    ;;
  *)
    echo "::error::memory_scope must be 'pr', 'branch', or 'repo'" >&2
    exit 1
    ;;
esac

scope_hash=$(hash_text "$scope_identity")
runner_hash=$(hash_text "$RUNNER_OS")
state_directory="$RUNNER_TEMP/dago-action-state/$agent_name/${scope}-${scope_hash}"
binary_directory="$RUNNER_TEMP/dago-action-bin/$runner_hash"
mkdir -p "$state_directory" "$binary_directory"

cache_prefix="dago-agent-${runner_hash}-${agent_name}-${scope}-${scope_hash}"
session_id="ci-${agent_name}-${scope_hash}"

{
  echo "workspace=$working_directory"
  echo "state_dir=$state_directory"
  echo "binary=$binary_directory/dacode"
  echo "session_id=$session_id"
  echo "cache_enabled=${INPUT_ENABLE_MEMORY:-true}"
  echo "cache_prefix=$cache_prefix"
  echo "cache_key=${cache_prefix}-${GITHUB_RUN_ID}-${GITHUB_RUN_ATTEMPT:-1}"
} >> "$GITHUB_OUTPUT"

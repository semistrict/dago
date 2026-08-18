#!/usr/bin/env bash
set -euo pipefail

skills_repo="${INPUT_SKILLS_REPO:-}"
[ -n "$skills_repo" ] || exit 0

case "$skills_repo" in
  https://github.com/*)
    repository=${skills_repo#https://github.com/}
    ;;
  *)
    repository=$skills_repo
    ;;
esac

ref=""
if [[ "$repository" == *@* ]]; then
  ref=${repository##*@}
  repository=${repository%@*}
fi
repository=${repository%.git}

if ! [[ "$repository" =~ ^[A-Za-z0-9][A-Za-z0-9_.-]*/[A-Za-z0-9][A-Za-z0-9_.-]*$ ]]; then
  echo "::error::skills_repo must be owner/repository[@ref] or an https://github.com URL" >&2
  exit 1
fi
if [[ "$repository" == *..* ]]; then
  echo "::error::skills_repo contains an unsafe repository name" >&2
  exit 1
fi
if [ -n "$ref" ]; then
  if ! [[ "$ref" =~ ^[A-Za-z0-9][A-Za-z0-9._/-]{0,199}$ ]] ||
     [[ "$ref" == *..* || "$ref" == *//* || "$ref" == *@\{* || "$ref" == *.lock ]]; then
    echo "::error::skills_repo contains an unsafe ref" >&2
    exit 1
  fi
fi

workspace="${INPUT_WORKING_DIRECTORY:?working directory is required}"
if [ ! -d "$workspace" ]; then
  echo "::error::working directory does not exist" >&2
  exit 1
fi

clone_directory=$(mktemp -d "${RUNNER_TEMP:-${TMPDIR:-/tmp}}/dago-skills.XXXXXX")
stage_directory=$(mktemp -d "$workspace/.dago-skills.XXXXXX")
cleanup() {
  rm -rf "$clone_directory" "$stage_directory"
}
trap cleanup EXIT

clone_args=(gh repo clone "$repository" "$clone_directory" -- --depth 1)
if [ -n "$ref" ]; then
  clone_args+=(--branch "$ref")
fi
if ! GH_TOKEN="${INPUT_GITHUB_TOKEN:-}" "${clone_args[@]}"; then
  echo "::error::failed to clone skills_repo; verify the repository and token access" >&2
  exit 1
fi

skill_files=()
while IFS= read -r -d '' skill_file; do
  skill_files+=("$skill_file")
done < <(find "$clone_directory" -type f -name SKILL.md -print0)
if [ "${#skill_files[@]}" -eq 0 ]; then
  echo "::error::skills_repo contains no directories with SKILL.md" >&2
  exit 1
fi

skill_names=()
target_directory="$workspace/.deepagents/skills"
for skill_file in "${skill_files[@]}"; do
  skill_directory=$(dirname "$skill_file")
  skill_name=$(basename "$skill_directory")
  if ! [[ "$skill_name" =~ ^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$ ]]; then
    echo "::error::invalid skill directory name '$skill_name'" >&2
    exit 1
  fi
  for installed_name in "${skill_names[@]:-}"; do
    if [ "$installed_name" = "$skill_name" ]; then
      echo "::error::skills_repo contains duplicate skill name '$skill_name'" >&2
      exit 1
    fi
  done
  if [ -e "$target_directory/$skill_name" ]; then
    echo "::error::skill '$skill_name' already exists in the workspace" >&2
    exit 1
  fi
  if [ -n "$(find "$skill_directory" -type l -print -quit)" ]; then
    echo "::error::skill '$skill_name' contains a symbolic link" >&2
    exit 1
  fi
  skill_names+=("$skill_name")
  cp -R "$skill_directory" "$stage_directory/$skill_name"
done

mkdir -p "$target_directory"
for skill_name in "${skill_names[@]}"; do
  cp -R "$stage_directory/$skill_name" "$target_directory/$skill_name"
  echo "Installed skill: $skill_name"
done

#!/bin/sh
set -eu

repo_root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
python_graph=${LANGGRAPH_PYTHON_ROOT:-/Users/ramon/src/langgraph}
temporary=$(mktemp -d "${TMPDIR:-/tmp}/dago-checkpoint-interop.XXXXXX")
trap 'rm -rf "$temporary"' EXIT HUP INT TERM

uv run \
  --with-editable "$python_graph/libs/checkpoint" \
  --with-editable "$python_graph/libs/checkpoint-sqlite" \
  python "$repo_root/conformance/python/checkpoint_interop.py" \
  generate "$temporary/python-safe.sqlite"

DAGO_PYTHON_SQLITE_FIXTURE="$temporary/python-safe.sqlite" \
  go test "$repo_root/checkpoint/sqlite" -run TestReadsAndContinuesPinnedPythonSafeFixture -count=1

go run "$repo_root/internal/conformance/cmd/checkpoint-fixture" "$temporary/go-safe.sqlite"

uv run \
  --with-editable "$python_graph/libs/checkpoint" \
  --with-editable "$python_graph/libs/checkpoint-sqlite" \
  python "$repo_root/conformance/python/checkpoint_interop.py" \
  verify "$temporary/go-safe.sqlite" --thread go-safe

if test -n "${DAGO_POSTGRES_TEST_DSN:-}"; then
  uv run \
    --with-editable "$python_graph/libs/checkpoint" \
    --with-editable "$python_graph/libs/checkpoint-postgres" \
    --with 'psycopg[binary]' \
    python "$repo_root/conformance/python/checkpoint_postgres_interop.py" \
    generate "$DAGO_POSTGRES_TEST_DSN"

  DAGO_PYTHON_POSTGRES_FIXTURE_DSN="$DAGO_POSTGRES_TEST_DSN" \
    go test "$repo_root/checkpoint/postgres" -run TestReadsAndContinuesPinnedPythonSafeFixture -count=1

  go run "$repo_root/internal/conformance/cmd/checkpoint-fixture" postgres "$DAGO_POSTGRES_TEST_DSN"

  uv run \
    --with-editable "$python_graph/libs/checkpoint" \
    --with-editable "$python_graph/libs/checkpoint-postgres" \
    --with 'psycopg[binary]' \
    python "$repo_root/conformance/python/checkpoint_postgres_interop.py" \
    verify "$DAGO_POSTGRES_TEST_DSN"
fi

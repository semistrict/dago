#!/bin/sh
set -eu

manifest="${1:-docs/upstream-manifest.json}"
python3 - "$manifest" <<'PY'
import json
import os
import subprocess
import sys

manifest = json.load(open(sys.argv[1], encoding="utf-8"))
failed = False
for source in manifest["sources"]:
    environment = source.get("checkout_env")
    path = os.environ.get(environment, "") if environment else ""
    revision = source["revision"]
    if not path or revision.startswith("v"):
        continue
    try:
        actual = subprocess.check_output(["git", "-C", path, "rev-parse", "HEAD"], text=True).strip()
    except (OSError, subprocess.CalledProcessError):
        print(f"missing reference checkout: {source['name']} ({environment}={path})", file=sys.stderr)
        failed = True
        continue
    if actual != revision:
        print(f"upstream drift: {source['name']} expected {revision}, found {actual}", file=sys.stderr)
        failed = True
if failed:
    raise SystemExit(1)
PY

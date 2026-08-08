#!/usr/bin/env python3
"""Generate and verify the safe cross-language SQLite checkpoint fixture.

Run this with the pinned LangGraph checkpoint and SQLite packages on PYTHONPATH.
The fixture deliberately contains only plain data, the shared message record, and
LangGraph's delta snapshot extension. It contains no constructor or pickle values.
"""

from __future__ import annotations

import argparse
import sqlite3
from pathlib import Path

from langgraph.checkpoint.serde.types import _DeltaSnapshot
from langgraph.checkpoint.sqlite import SqliteSaver


PYTHON_CHECKPOINT_ID = "1f000000-0000-6000-8000-000000000001"


def checkpoint(checkpoint_id: str, channel_values: dict[str, object]) -> dict[str, object]:
    return {
        "v": 4,
        "id": checkpoint_id,
        "ts": "2026-08-08T12:00:00+00:00",
        "channel_values": channel_values,
        "channel_versions": {key: "00000000000000000000000000000001.0.1" for key in channel_values},
        "versions_seen": {"writer": {key: "00000000000000000000000000000001.0.1" for key in channel_values}},
        "updated_channels": list(channel_values),
    }


def safe_values() -> dict[str, object]:
    message = {
        "$type": "dago.message.v1",
        "id": "human-1",
        "role": "human",
        "content": [{"type": "text", "text": "hello from Python"}],
        "metadata": {"fixture": True},
    }
    return {
        "scalar": "python",
        "bytes": b"\x00\x01safe",
        "nested": {"items": [1, True, None, 3.25]},
        "messages": [message],
        "delta": _DeltaSnapshot(["seed", {"count": 1}]),
    }


def open_saver(database_path: Path) -> tuple[sqlite3.Connection, SqliteSaver]:
    connection = sqlite3.connect(database_path, check_same_thread=False)
    saver = SqliteSaver(connection)
    saver.setup()
    return connection, saver


def generate(database_path: Path) -> None:
    if database_path.exists():
        database_path.unlink()
    database_path.parent.mkdir(parents=True, exist_ok=True)
    connection, saver = open_saver(database_path)
    try:
        root = {"configurable": {"thread_id": "python-safe", "checkpoint_ns": ""}}
        saved = saver.put(
            root,
            checkpoint(PYTHON_CHECKPOINT_ID, safe_values()),
            {"source": "input", "step": 0, "fixture_owner": "python"},
            {},
        )
        saver.put_writes(
            saved,
            [
                ("delta", ["write-one"]),
                ("plain", {"source": "python", "ok": True}),
                ("binary", b"pending-bytes"),
            ],
            "python-task",
            "ignored-by-sqlite",
        )
    finally:
        connection.close()


def verify(database_path: Path, thread_id: str) -> None:
    connection, saver = open_saver(database_path)
    try:
        loaded = saver.get_tuple({"configurable": {"thread_id": thread_id, "checkpoint_ns": ""}})
        assert loaded is not None, "checkpoint is missing"
        values = loaded.checkpoint["channel_values"]
        assert values["scalar"] in {"python", "go"}
        assert values["bytes"] == b"\x00\x01safe"
        assert values["messages"][0]["$type"] == "dago.message.v1"
        assert isinstance(values["delta"], _DeltaSnapshot)
        assert loaded.pending_writes, "pending writes are missing"

        continued = dict(loaded.checkpoint)
        continued["id"] = "1f000002-0000-6000-8000-000000000001"
        continued["ts"] = "2026-08-08T12:02:00+00:00"
        continued["channel_values"] = dict(values)
        continued["channel_values"]["continued_by_python"] = True
        saver.put(loaded.config, continued, {"source": "loop", "step": 2}, {})
    finally:
        connection.close()


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("mode", choices=("generate", "verify"))
    parser.add_argument("database", type=Path)
    parser.add_argument("--thread", default="go-safe")
    arguments = parser.parse_args()
    if arguments.mode == "generate":
        generate(arguments.database)
    else:
        verify(arguments.database, arguments.thread)


if __name__ == "__main__":
    main()

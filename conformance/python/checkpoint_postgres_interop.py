#!/usr/bin/env python3
"""Generate and verify safe cross-language PostgreSQL checkpoints."""

from __future__ import annotations

import argparse

from langgraph.checkpoint.postgres import PostgresSaver
from langgraph.checkpoint.serde.types import _DeltaSnapshot


def checkpoint(checkpoint_id: str, channel_values: dict[str, object]) -> dict[str, object]:
    versions = {
        key: "00000000000000000000000000000001.0.1" for key in channel_values
    }
    return {
        "v": 4,
        "id": checkpoint_id,
        "ts": "2026-08-08T12:10:00+00:00",
        "channel_values": channel_values,
        "channel_versions": versions,
        "versions_seen": {"writer": versions},
        "updated_channels": list(channel_values),
    }


def safe_values() -> dict[str, object]:
    return {
        "scalar": "python",
        "bytes": b"\x00\x01safe",
        "nested": {"items": [1, True, None, 3.25]},
        "messages": [
            {
                "$type": "dago.message.v1",
                "id": "human-1",
                "role": "human",
                "content": [{"type": "text", "text": "hello from Python"}],
                "metadata": {"fixture": True},
            }
        ],
        "delta": _DeltaSnapshot(["seed", {"count": 1}]),
    }


def generate(dsn: str) -> None:
    with PostgresSaver.from_conn_string(dsn) as saver:
        saver.setup()
        saver.delete_thread("python-safe-postgres")
        root = {"configurable": {"thread_id": "python-safe-postgres", "checkpoint_ns": ""}}
        value = checkpoint("1f000010-0000-6000-8000-000000000001", safe_values())
        saved = saver.put(
            root,
            value,
            {"source": "input", "step": 0, "fixture_owner": "python"},
            value["channel_versions"],
        )
        saver.put_writes(
            saved,
            [
                ("delta", ["write-one"]),
                ("plain", {"source": "python", "ok": True}),
                ("binary", b"pending-bytes"),
            ],
            "python-task",
            "00000000/python",
        )


def verify(dsn: str) -> None:
    with PostgresSaver.from_conn_string(dsn) as saver:
        saver.setup()
        loaded = saver.get_tuple(
            {"configurable": {"thread_id": "go-safe-postgres", "checkpoint_ns": ""}}
        )
        assert loaded is not None, "Go checkpoint is missing"
        values = loaded.checkpoint["channel_values"]
        assert values["scalar"] == "go"
        assert values["bytes"] == b"\x00\x01safe"
        assert values["messages"][0]["$type"] == "dago.message.v1"
        assert loaded.pending_writes, "Go pending writes are missing"

        continued = dict(loaded.checkpoint)
        continued["id"] = "1f000012-0000-6000-8000-000000000001"
        continued["ts"] = "2026-08-08T12:12:00+00:00"
        continued["channel_values"] = dict(values)
        continued["channel_values"]["continued_by_python"] = True
        saver.put(loaded.config, continued, {"source": "loop", "step": 2}, {})


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("mode", choices=("generate", "verify"))
    parser.add_argument("dsn")
    arguments = parser.parse_args()
    if arguments.mode == "generate":
        generate(arguments.dsn)
    else:
        verify(arguments.dsn)


if __name__ == "__main__":
    main()

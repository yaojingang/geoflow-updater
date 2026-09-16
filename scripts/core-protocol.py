#!/usr/bin/env python3
"""Derive the release floor from the immutable Core checkout, never its version string."""
from __future__ import annotations

import argparse
import json
from pathlib import Path

CONTRACT = {
    "schema_version": 1,
    "minimum_updater_protocol": 5,
    "recovery_state_schema": 1,
    "host_api_protocol": 2,
    "management_protocol": "1.0",
}


def pairs_unique(pairs):
    result = {}
    for key, value in pairs:
        if key in result:
            raise ValueError("duplicate recovery contract field")
        result[key] = value
    return result


def minimum_protocol(core: Path, strategy: str) -> int:
    if strategy not in {"maintenance", "online"}:
        raise ValueError("unsupported upgrade strategy")
    declaration = core / "deployment" / "recovery-contract.json"
    if declaration.is_symlink():
        raise ValueError("recovery contract must be a regular source file")
    if not declaration.exists():
        return 3 if strategy == "maintenance" else 4
    if not declaration.is_file() or declaration.stat().st_size > 4096:
        raise ValueError("invalid recovery contract file")
    value = json.loads(declaration.read_text(encoding="utf-8"), object_pairs_hook=pairs_unique)
    # Exact types prevent JSON booleans from matching integer protocol values.
    if not isinstance(value, dict) or value.keys() != CONTRACT.keys() or any(
        type(value[key]) is not type(expected) or value[key] != expected
        for key, expected in CONTRACT.items()
    ):
        raise ValueError("unsupported Core recovery contract")
    if strategy != "maintenance":
        raise ValueError("coordinated Core online upgrade requires separate version-pair acceptance")
    return 5


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("core", type=Path)
    parser.add_argument("strategy", choices=["maintenance", "online"])
    args = parser.parse_args()
    try:
        print(minimum_protocol(args.core, args.strategy))
    except (OSError, ValueError) as error:
        parser.exit(1, f"Core protocol declaration rejected: {error}\n")


if __name__ == "__main__":
    main()

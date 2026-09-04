#!/usr/bin/env python3
"""Diagnostic branch only: emit classifications, never source text or secrets."""
import json
import pathlib
import re
import sys

source_kind, log_path = sys.argv[1:]
if source_kind not in {"updater-journal", "container-output", "application-log"}:
    raise SystemExit("Unsupported diagnostic source category")
current_code, *patterns = sys.stdin.buffer.read().split(b"\0")
patterns = sorted(set(filter(None, patterns)), key=len, reverse=True)
matches = []
for line_number, line in enumerate(pathlib.Path(log_path).read_bytes().splitlines(), 1):
    for pattern in patterns:
        position = line.find(pattern)
        if position < 0:
            continue
        before = line[position - 1:position] if position else b""
        after = line[position + len(pattern):position + len(pattern) + 1]
        frame_methods = [name for name in (
            "startRollback", "startAuthorizedMutation", "startOperation",
            "instanceRequest", "request", "report",
        ) if (name + "(").encode() in line]
        matches.append({
            "source_kind": source_kind,
            "line_number": line_number,
            "value_shape": "six-digit" if re.fullmatch(rb"[0-9]{6}", pattern) else "other-sensitive-value",
            "matches_current_rollback_code": bool(current_code and pattern == current_code),
            "php_stack_frame": bool(re.match(rb"^#\d+\s", line)),
            "known_frame_methods": frame_methods,
            "quoted_value": before in (b"'", b'"') and after == before,
            "embedded_in_digits": before.isdigit() or after.isdigit(),
        })
        if len(matches) >= 100:
            break
    if len(matches) >= 100:
        break
print(json.dumps({"matches": matches, "truncated": len(matches) >= 100}))

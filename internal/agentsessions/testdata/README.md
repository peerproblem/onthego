# Native transcript fixtures

These fixtures were generated on 2026-10-16 by installed Codex CLI 0.163.0 and Claude Code 2.1.301 in isolated temporary agent homes. Each CLI connected to a local deterministic HTTP provider and completed a harmless marker conversation. No existing user session, credential file, global history index or database was used. Paths were sanitized to `/fixture/project`, `/fixture/runtime` and `/fixture/user`; native JSONL record structure and ordering were retained.

The same verification run used the production ExportWithOptions and Import adapters with different source and destination workspace paths, then resumed each transcript from a second, initially empty agent home containing JSONL only. The local provider asserted that the resumed request contained both the original user marker and the original assistant response. The provider script and reproducible commands live in [`scripts/agent-compatibility/verify.py`](../../../scripts/agent-compatibility/verify.py).

Tests load these files to verify metadata extraction, byte-preserving export and remapping of recognized path tokens. Message bodies and embedded protocol strings remain opaque. These fixture tests alone do not establish compatibility with untested CLI releases.

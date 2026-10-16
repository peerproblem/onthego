#!/usr/bin/env python3
"""Exercise real agent CLIs with isolated state and a local deterministic provider.

This verifies transcript persistence and JSONL-only resume, not hosted-model access.
No credentials, user sessions, global settings, or SQLite databases are copied.
"""

import argparse
import http.server
import json
import os
from pathlib import Path
import re
import shutil
import subprocess
import tempfile
import threading
import time
import uuid


SEED = "ONTHEGO_NATIVE_SESSION_7c28"
FIRST = "ONTHEGO_SESSION_SAVED"
RESUMED = "ONTHEGO_SESSION_RESUMED"


class Provider(http.server.BaseHTTPRequestHandler):
    requests = []

    def log_message(self, *_args):
        pass

    def do_POST(self):
        body = json.loads(self.rfile.read(int(self.headers.get("Content-Length", "0"))))
        if "count_tokens" in self.path:
            data = b'{"input_tokens":100}'
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(data)))
            self.end_headers()
            self.wfile.write(data)
            return
        messages = body.get("input", body.get("messages", []))
        content = json.dumps(messages)
        resumed = "Resume verification" in content
        restored = SEED in content and FIRST in content
        Provider.requests.append({"path": self.path, "resumed": resumed, "restored": restored})
        answer = RESUMED if resumed and restored else ("MISSING_CONTEXT" if resumed else FIRST)
        if "/responses" in self.path:
            item = {"id": "msg_test", "type": "message", "role": "assistant", "status": "completed", "content": [{"type": "output_text", "text": answer, "annotations": []}]}
            response = {"id": "resp_test", "object": "response", "status": "completed", "output": [item], "usage": {"input_tokens": 100, "output_tokens": 10, "total_tokens": 110}}
            events = [
                {"type": "response.created", "response": {**response, "status": "in_progress", "output": []}},
                {"type": "response.output_item.added", "output_index": 0, "item": {**item, "content": [], "status": "in_progress"}},
                {"type": "response.content_part.added", "item_id": "msg_test", "output_index": 0, "content_index": 0, "part": {"type": "output_text", "text": "", "annotations": []}},
                {"type": "response.output_text.delta", "item_id": "msg_test", "output_index": 0, "content_index": 0, "delta": answer},
                {"type": "response.output_text.done", "item_id": "msg_test", "output_index": 0, "content_index": 0, "text": answer},
                {"type": "response.output_item.done", "output_index": 0, "item": item},
                {"type": "response.completed", "response": response},
            ]
        elif "/messages" in self.path:
            events = [
                {"type": "message_start", "message": {"id": "msg_test", "type": "message", "role": "assistant", "model": body["model"], "content": [], "stop_reason": None, "stop_sequence": None, "usage": {"input_tokens": 100, "output_tokens": 0}}},
                {"type": "content_block_start", "index": 0, "content_block": {"type": "text", "text": ""}},
                {"type": "content_block_delta", "index": 0, "delta": {"type": "text_delta", "text": answer}},
                {"type": "content_block_stop", "index": 0},
                {"type": "message_delta", "delta": {"stop_reason": "end_turn", "stop_sequence": None}, "usage": {"output_tokens": 10}},
                {"type": "message_stop"},
            ]
        else:
            self.send_error(404)
            return
        data = "".join(f"event: {event['type']}\ndata: {json.dumps(event)}\n\n" for event in events).encode()
        self.send_response(200)
        self.send_header("Content-Type", "text/event-stream")
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)


def run(command, cwd, env):
    result = subprocess.run(command, cwd=cwd, env=env, text=True, capture_output=True, timeout=90)
    if result.returncode:
        raise RuntimeError(f"{Path(command[0]).name} failed ({result.returncode}):\n{result.stdout}\n{result.stderr}")
    return result.stdout


def environment():
    # Explicit allowlist prevents inheritance of real API tokens and agent settings.
    return {key: value for key, value in os.environ.items() if key in ("PATH", "HOME", "TMPDIR", "SYSTEMROOT", "LANG")}


def codex_config(home, url, instruction_file):
    home.mkdir(parents=True)
    (home / "config.toml").write_text(
        'model = "gpt-5.4"\nmodel_provider = "compatibility"\n'
        f'model_instructions_file = "{instruction_file}"\n'
        '[model_providers.compatibility]\nname = "Local compatibility fixture"\n'
        f'base_url = "{url}/v1"\nwire_api = "responses"\n'
        'requires_openai_auth = false\nsupports_websockets = false\n'
        'request_max_retries = 0\nstream_max_retries = 0\n'
    )


def sanitized(data, workspace, root):
    # Preserve native JSONL bytes apart from fixture-specific host paths.
    return data.replace(str(workspace).encode(), b"/fixture/project").replace(str(root).encode(), b"/fixture/runtime").replace(str(Path.home()).encode(), b"/fixture/user")


def verify_codex(binary, root, url, output, adapter, golden=None):
    workspace = root / "codex-project"
    workspace.mkdir()
    target_workspace = root / "codex-destination"
    target_workspace.mkdir()
    instructions = root / "instructions.md"
    instructions.write_text("This is an isolated transcript compatibility test. Do not run tools.\n")
    source, target = root / "codex-source", root / "codex-target"
    codex_config(source, url, instructions)
    codex_config(target, url, instructions)
    env = {**environment(), "CODEX_HOME": str(source)}
    base = [binary, "exec", "--skip-git-repo-check", "--ignore-rules", "--json"]
    started = time.monotonic()
    if golden:
        native = golden.read_bytes().replace(b"/fixture/project", str(workspace).encode())
        session_id = json.loads(native.splitlines()[0])["payload"]["id"]
        initial = source / "sessions" / "2026" / "10" / "16" / f"rollout-2026-10-16T00-00-00-{session_id}.jsonl"
        initial.parent.mkdir(parents=True)
        initial.write_bytes(native)
    else:
        first = run(base + [f"Remember {SEED}."], workspace, env)
        assert FIRST in first, first
    logs = list((source / "sessions").rglob("*.jsonl"))
    assert len(logs) == 1, logs
    native = logs[0].read_bytes()
    session_id = json.loads(native.splitlines()[0])["payload"]["id"]
    run([str(adapter), str(workspace), str(target_workspace), str(source), str(target), str(root / "codex-bundle")], root, environment())
    assert not list(target.glob("*.sqlite*")) and not (target / "auth.json").exists()
    resumed = run([binary, "exec", "resume", "--skip-git-repo-check", "--ignore-rules", "--json", session_id, "Resume verification."], target_workspace, {**env, "CODEX_HOME": str(target)})
    assert RESUMED in resumed, resumed
    version = run([binary, "--version"], workspace, env).strip()
    if output and not golden:
        (output / "codex-native.jsonl").write_bytes(sanitized(native, workspace, root))
    return {"agent": "codex", "version": version, "fixture": golden.name if golden else "generated", "resume": "passed", "production_adapter": True, "workspace_remapped": True, "jsonl_only": True, "database_copied": False, "elapsed_seconds": round(time.monotonic() - started, 3), "session_id": session_id}


def claude_project_directory(path):
    """Claude 2.1.301 encoding, observed from native CLI and packaged implementation."""
    code_units = [int.from_bytes(path.encode("utf-16-le")[i:i + 2], "little") for i in range(0, len(path.encode("utf-16-le")), 2)]
    encoded = "".join(chr(unit) if (48 <= unit <= 57 or 65 <= unit <= 90 or 97 <= unit <= 122) else "-" for unit in code_units)
    if len(encoded) <= 200:
        return encoded
    value = 0
    for unit in code_units:
        value = (value * 31 + unit) & 0xffffffff
    value = abs(value if value < 0x80000000 else value - 0x100000000)
    digits = "0123456789abcdefghijklmnopqrstuvwxyz"
    suffix = ""
    while value:
        suffix = digits[value % 36] + suffix
        value //= 36
    return encoded[:200] + "-" + (suffix or "0")


def verify_claude(binary, root, url, output, adapter, golden=None):
    # Deliberately exceed 200 characters to exercise Claude's long-path encoding.
    workspace = root / "claude-project" / ("long-segment-" * 8) / ("nested-segment-" * 8)
    workspace.mkdir(parents=True)
    target_workspace = root / "claude-destination" / ("different-segment-" * 8) / ("resumed-segment-" * 8)
    target_workspace.mkdir(parents=True)
    source, target = root / "claude-source", root / "claude-target"
    source.mkdir()
    target.mkdir()
    env = {**environment(), "CLAUDE_CONFIG_DIR": str(source), "ANTHROPIC_BASE_URL": url, "ANTHROPIC_API_KEY": "local-fixture-key", "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC": "1", "DISABLE_AUTOUPDATER": "1", "DISABLE_TELEMETRY": "1"}
    session_id = str(uuid.uuid4())
    base = [binary, "--bare", "-p", "--model", "claude-sonnet-4-6", "--tools", "", "--strict-mcp-config", "--system-prompt", "Isolated transcript test. Do not use tools."]
    started = time.monotonic()
    if golden:
        native = golden.read_bytes().replace(b"/fixture/project", str(workspace).encode())
        session_id = next(json.loads(line)["sessionId"] for line in native.splitlines() if "sessionId" in json.loads(line))
        initial = source / "projects" / claude_project_directory(str(workspace)) / f"{session_id}.jsonl"
        initial.parent.mkdir(parents=True)
        initial.write_bytes(native)
    else:
        first = run(base + ["--session-id", session_id, f"Remember {SEED}."], workspace, env)
        assert FIRST in first, first
    logs = list((source / "projects").rglob(session_id + ".jsonl"))
    assert len(logs) == 1, logs
    native = logs[0].read_bytes()
    run([str(adapter), str(workspace), str(target_workspace), str(source), str(target), str(root / "claude-bundle")], root, environment())
    resumed = run(base + ["--resume", session_id, "Resume verification."], target_workspace, {**env, "CLAUDE_CONFIG_DIR": str(target)})
    assert RESUMED in resumed, resumed
    version = run([binary, "--version"], workspace, env).strip()
    encoded = logs[0].parent.name
    assert encoded == claude_project_directory(str(workspace)), encoded
    ordinary = re.sub(r"[^a-zA-Z0-9]", "-", str(workspace.resolve()))
    if output and not golden:
        (output / "claude-native.jsonl").write_bytes(sanitized(native, workspace, root))
    return {"agent": "claude", "version": version, "fixture": golden.name if golden else "generated", "resume": "passed", "production_adapter": True, "workspace_remapped": True, "jsonl_only": True, "database_copied": False, "elapsed_seconds": round(time.monotonic() - started, 3), "session_id": session_id, "long_path": {"encoded_length": len(encoded), "ordinary_length": len(ordinary), "encoding_matched": True}}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--codex", default=shutil.which("codex"))
    parser.add_argument("--claude", default=shutil.which("claude"))
    parser.add_argument("--fixtures-output", type=Path)
    args = parser.parse_args()
    if not args.codex or not args.claude:
        parser.error("both CLIs are required; provide --codex and --claude paths")
    if args.fixtures_output:
        args.fixtures_output.mkdir(parents=True, exist_ok=True)
    server = http.server.ThreadingHTTPServer(("127.0.0.1", 0), Provider)
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    try:
        with tempfile.TemporaryDirectory(prefix="onthego-native-compat-") as temporary:
            root = Path(temporary).resolve()
            adapter = root / "session-adapter"
            repository = Path(__file__).resolve().parents[2]
            run(["go", "build", "-o", str(adapter), "./scripts/agent-compatibility"], repository, environment())
            url = f"http://127.0.0.1:{server.server_port}"
            results = [verify_codex(args.codex, root, url, args.fixtures_output, adapter), verify_claude(args.claude, root, url, args.fixtures_output, adapter)]
            golden_root = root / "golden"
            golden_root.mkdir()
            fixtures = repository / "internal/agentsessions/testdata"
            results += [verify_codex(args.codex, golden_root, url, None, adapter, fixtures / "codex-0.163.0.jsonl"), verify_claude(args.claude, golden_root, url, None, adapter, fixtures / "claude-2.1.301.jsonl")]
            resumed = [request for request in Provider.requests if request["resumed"]]
            assert len(resumed) >= 4 and all(request["restored"] for request in resumed), resumed
            report = {"mode": "real-cli-local-provider", "hosted_model_tested": False, "results": results, "resume_requests": resumed}
            if args.fixtures_output:
                (args.fixtures_output / "verification.json").write_text(json.dumps(report, indent=2) + "\n")
            print(json.dumps(report, indent=2))
    finally:
        server.shutdown()
        server.server_close()


if __name__ == "__main__":
    main()

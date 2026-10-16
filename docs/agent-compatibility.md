# Native agent compatibility

ONTHEGO transfers project-scoped native transcripts. It does not transfer authentication, global settings, session-list indexes, or SQLite databases. Resume by the session ID printed after a handoff. Desktop sidebars and IDE session pickers are outside the verified contract.

## Verified versions

Verified on macOS arm64 on 2026-10-16. These were the current `latest` versions reported by the official npm packages that day:

| Agent | Version | Native generation | Resume after production export/import | Fresh destination home | Committed macOS fixture resume |
| --- | --- | --- | --- | --- | --- |
| [Codex CLI](https://www.npmjs.com/package/@openai/codex) | 0.163.0 | Passed | Passed | JSONL only, no copied database or auth | Passed |
| [Claude Code](https://www.npmjs.com/package/@anthropic-ai/claude-code) | 2.1.301 | Passed | Passed | JSONL only, no copied database or auth | Passed |

`onthego doctor` reports installed versions and warns outside this exact verified set. A warning means unverified, not known incompatible. Re-run the harness before extending this table or the version checks.

The test uses the **real installed CLI binaries** with a deterministic HTTP provider bound to `127.0.0.1`. The provider substitutes model inference only. Each CLI creates its own native transcript. ONTHEGO's actual `agentsessions.ExportWithOptions` and `agentsessions.Import` move that transcript into a different agent home and remap it to a different workspace. The CLI then resumes it by ID. The provider checks that the resumed request contains both the original user marker and the original assistant response before returning the success marker.

This proves native transcript parsing and conversation restoration through the production adapter. It does not verify a hosted model, account entitlement, Internet latency, interactive terminal selection, or desktop integration. No real credential is read or copied. Claude runs with `--bare`, which excludes keychain access and user customizations. The fixed Anthropic API key is a local test string accepted only by the fixture server. Codex uses a local custom provider with authentication disabled.

## Reproduce

Requirements: Go matching `go.mod`, Python 3, Node.js, npm, and the two CLI binaries. With binaries on `PATH`, run from the repository root:

```sh
python3 scripts/agent-compatibility/verify.py
```

To install exact versions in an isolated location:

```sh
agent_cli_dir="$(mktemp -d)"
npm install --prefix "$agent_cli_dir" @openai/codex@0.163.0 @anthropic-ai/claude-code@2.1.301 --no-audit --no-fund
python3 scripts/agent-compatibility/verify.py \
  --codex "$agent_cli_dir/node_modules/.bin/codex" \
  --claude "$agent_cli_dir/node_modules/.bin/claude"
rm -rf "$agent_cli_dir"
```

If local npm policy blocks Claude's postinstall, review and run the package's documented installer before executing the harness:

```sh
node "$agent_cli_dir/node_modules/@anthropic-ai/claude-code/install.cjs"
```

The harness builds its small Go adapter bridge in a temporary directory. It creates separate source and destination agent homes, invokes both native CLIs, and removes all temporary transcripts on exit. It also imports and resumes the committed macOS-generated fixtures from [`internal/agentsessions/testdata`](../internal/agentsessions/testdata/README.md). Running that same harness on Linux verifies that Linux CLIs can resume these macOS-generated fixtures after path remapping. The macOS and Ubuntu CI matrix executes the harness. Platform claims require a passing run for that platform, not merely the presence of the matrix configuration.

The final JSON report records CLI versions, each resume outcome, elapsed time, and whether both prior messages reached the resumed provider request. A nonzero exit means the gate failed. `--fixtures-output /path/to/output` additionally writes newly generated sanitized JSONL and the report for fixture review. It never reads existing user transcripts. Replace fixtures only after reviewing the generated files for host-specific data, and update their provenance alongside them.

Check the current registry versions separately:

```sh
npm view @openai/codex version
npm view @anthropic-ai/claude-code version
```

## Observed results and limits

The local macOS run on 2026-10-16 measured 0.906 seconds for Codex and 0.669 seconds for Claude from initial native generation through successful resume. Importing and resuming the committed golden fixtures took 0.363 and 0.199 seconds respectively. All four resumed requests included both prior messages. These are one-run compatibility timings with a localhost provider. They exclude installing binaries and compiling the bridge. They are not SSH handoff latency or hosted-model performance measurements.

Codex 0.163.0 successfully resumed an imported rollout by UUID with no source SQLite file or auth file in the destination home. Codex may create its own local state database during startup. This test establishes that **copying the source database is unnecessary for explicit-ID CLI resume**; it makes no claim that Codex never uses a database.

Claude 2.1.301 was tested with both source and destination workspace paths longer than 300 characters. Its project directory name follows this observed rule:

1. Replace each non-ASCII-alphanumeric UTF-16 code unit in the full workspace path with `-`.
2. If the result is at most 200 characters, use it unchanged.
3. Otherwise, keep the first 200 characters and append `-` plus the base-36 absolute value of a signed 32-bit hash of the original path. Starting from zero, update the hash with `hash = (31 * hash + UTF16_code_unit) | 0` for each code unit.

The rule was checked against the actual directory created by the installed CLI and the matching implementation shipped in its native npm binary. The adapter includes Unicode coverage so non-BMP characters follow JavaScript's UTF-16 behavior. Claude storage names are an internal implementation detail, not a promised upstream API.

The official [Codex configuration reference](https://developers.openai.com/codex/config-reference/) documents custom provider configuration. Claude documents its [gateway endpoint configuration](https://code.claude.com/docs/en/llm-gateway) and [resume and bare CLI options](https://code.claude.com/docs/en/cli-reference). These document the test entry points; the checked-in harness and native fixtures provide the evidence for transcript compatibility.

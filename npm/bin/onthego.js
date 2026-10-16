#!/usr/bin/env node

const { spawn } = require("node:child_process");
const { existsSync } = require("node:fs");
const path = require("node:path");

const supported = new Map([
  ["darwin-arm64", "onthego-darwin-arm64"],
  ["darwin-x64", "onthego-darwin-amd64"],
  ["linux-x64", "onthego-linux-amd64"],
  ["linux-arm64", "onthego-linux-arm64"],
]);

const platform = `${process.platform}-${process.arch}`;
const filename = supported.get(platform);
if (!filename) {
  process.stderr.write(`onthego does not support ${platform} yet\n`);
  process.exit(1);
}

const binary = path.resolve(__dirname, "..", "vendor", filename);
if (!existsSync(binary)) {
  process.stderr.write(`onthego binary is missing for ${platform}\n`);
  process.exit(1);
}

const child = spawn(binary, process.argv.slice(2), {
  stdio: "inherit",
  env: process.env,
  // Supported platforms are POSIX. A separate group prevents a terminal or
  // group signal from reaching the child both directly and via forwarding.
  // Keep the child referenced so this launcher always waits for cleanup.
  detached: true,
});

const handlers = new Map();
for (const signal of ["SIGINT", "SIGTERM", "SIGHUP"]) {
  const forward = () => child.kill(signal);
  handlers.set(signal, forward);
  process.on(signal, forward);
}

function removeHandlers() {
  for (const [signal, handler] of handlers) {
    process.removeListener(signal, handler);
  }
}

child.once("error", (error) => {
  removeHandlers();
  process.stderr.write(`${error.message}\n`);
  process.exit(1);
});

child.once("exit", (code, signal) => {
  removeHandlers();
  if (signal) {
    process.kill(process.pid, signal);
    return;
  }
  process.exit(code ?? 1);
});

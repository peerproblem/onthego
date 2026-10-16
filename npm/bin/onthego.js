#!/usr/bin/env node

const { spawnSync } = require("node:child_process");
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

const result = spawnSync(binary, process.argv.slice(2), {
  stdio: "inherit",
  env: process.env,
});

if (result.error) {
  process.stderr.write(`${result.error.message}\n`);
  process.exit(1);
}
process.exit(result.status ?? 1);

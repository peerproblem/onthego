import { access, constants, readFile } from "node:fs/promises";
import { spawnSync } from "node:child_process";

const binaries = [
  "npm/vendor/onthego-darwin-arm64",
  "npm/vendor/onthego-darwin-amd64",
  "npm/vendor/onthego-linux-arm64",
  "npm/vendor/onthego-linux-amd64",
];
const expected = new Set([
  "package.json",
  "README.md",
  "LICENSE",
  "npm/bin/onthego.js",
  ...binaries,
]);

for (const file of ["npm/bin/onthego.js", ...binaries]) {
  await access(file, constants.R_OK | constants.X_OK);
}
for (const file of binaries) {
  const bytes = await readFile(file);
  const elf = bytes.subarray(0, 4).equals(Buffer.from([0x7f, 0x45, 0x4c, 0x46]));
  const macho = bytes.subarray(0, 4).equals(Buffer.from([0xcf, 0xfa, 0xed, 0xfe]));
  if (file.includes("linux") ? !elf : !macho) {
    throw new Error(`${file} is not the expected executable format`);
  }
}

// Inspect npm's actual allowlist without recursively invoking this prepack hook.
const result = spawnSync("npm", ["pack", "--dry-run", "--ignore-scripts", "--json"], {
  encoding: "utf8",
});
if (result.status !== 0) {
  throw new Error(`npm package inspection failed: ${result.stderr}`);
}
const [pack] = JSON.parse(result.stdout);
const actual = new Set(pack.files.map(({ path }) => path));
for (const file of actual) {
  if (!expected.has(file)) throw new Error(`Unexpected package file: ${file}`);
}
for (const file of expected) {
  if (!actual.has(file)) throw new Error(`Missing package file: ${file}`);
}
console.log(`Validated ${actual.size} package files and ${binaries.length} platform binaries.`);

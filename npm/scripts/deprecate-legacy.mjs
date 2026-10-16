import { execFileSync } from "node:child_process";
import { readFile } from "node:fs/promises";

const { name } = JSON.parse(await readFile(new URL("../../package.json", import.meta.url)));
const versions = JSON.parse(execFileSync("npm", ["view", name, "versions", "--json"], { encoding: "utf8" }));
const legacy = (Array.isArray(versions) ? versions : [versions]).filter((version) => /^0\.1\./.test(version));
const message = "0.1 is retired and its central server is discontinued. Upgrade to 0.3 for direct device-to-device handoffs; see the migration notes in README.";
let updated = 0;
for (const version of legacy) {
  const output = execFileSync("npm", ["view", `${name}@${version}`, "deprecated", "--json"], { encoding: "utf8" }).trim();
  const existing = output ? JSON.parse(output) : null;
  const notices = Array.isArray(existing) ? existing : [existing];
  if (notices.length === 1 && notices[0] === message) continue;
  execFileSync("npm", ["deprecate", `${name}@${version}`, message], { stdio: "inherit" });
  updated++;
}
console.log(`Deprecated ${updated} existing 0.1 releases; ${legacy.length - updated} already had the retirement notice.`);

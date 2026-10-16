import { execFileSync } from "node:child_process";
import { readFile } from "node:fs/promises";

const { name } = JSON.parse(await readFile(new URL("../../package.json", import.meta.url)));
const versions = JSON.parse(execFileSync("npm", ["view", name, "versions", "--json"], { encoding: "utf8" }));
const legacy = (Array.isArray(versions) ? versions : [versions]).filter((version) => /^0\.1\./.test(version));
for (const version of legacy) {
  execFileSync("npm", ["deprecate", `${name}@${version}`, "0.1 is retired and its central server is discontinued. Upgrade to 0.3 for direct device-to-device handoffs; see the migration notes in README."], { stdio: "inherit" });
}
console.log(`Deprecated ${legacy.length} existing 0.1 releases.`);

#!/usr/bin/env node
// Proves the vendored ClayJS runtime is byte-identical to the published npm release of
// the locked version. The lock records a version only, so without this a build labeled
// 1.15.2 could ship commits past the real 1.15.2 and nothing would notice.
import { execFileSync } from "node:child_process";
import { createHash } from "node:crypto";
import { mkdtemp, readFile, readdir, rm } from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { isDeepStrictEqual } from "node:util";

const NAME = "check-clay-runtime-release";
const ROOT = path.resolve(fileURLToPath(new URL("..", import.meta.url)));
const LOCK_FILE = path.join(ROOT, "offline-runtime.lock.json");
const ASSETS = path.join(ROOT, "internal", "clayruntime", "assets");
const PACKAGE = "@panphora/clayjs";

const sha256 = (bytes) => createHash("sha256").update(bytes).digest("hex");

const USAGE = `usage: node scripts/${NAME}.mjs [--pack-dir <dir>] [--bundle-root <dir>]`;

function fail(message) {
  console.error(`${NAME}: ${message}`);
  process.exit(1);
}

async function publishedPackage(version, packDir, tempDirs) {
  if (packDir) return path.join(packDir, version, "package");
  const tmp = await mkdtemp(path.join(os.tmpdir(), `${NAME}-${version}-`));
  tempDirs.push(tmp);
  execFileSync("npm", ["pack", `${PACKAGE}@${version}`, "--pack-destination", tmp, "--silent", "--min-release-age=0"], {
    cwd: tmp,
    stdio: ["ignore", "pipe", "pipe"],
  });
  const tarball = (await readdir(tmp)).find((entry) => entry.endsWith(".tgz"));
  if (!tarball) throw new Error(`npm pack ${PACKAGE}@${version} produced no tarball`);
  execFileSync("tar", ["-xzf", path.join(tmp, tarball), "-C", tmp], { stdio: ["ignore", "pipe", "pipe"] });
  return path.join(tmp, "package");
}

async function checkVersion(version, packageRoot, bundleRoot) {
  const problems = [];
  const bundledDir = path.join(bundleRoot, version);
  let bundledIndex;
  try {
    bundledIndex = JSON.parse(await readFile(path.join(bundledDir, "offline-manifest.json"), "utf8"));
  } catch (err) {
    problems.push(`the bundled ClayJS ${version} has no readable offline index at ${path.join(bundledDir, "offline-manifest.json")} (${err.message})`);
    return { problems, files: 0 };
  }

  let publishedBytes;
  try {
    publishedBytes = await readFile(path.join(packageRoot, "dist", "offline-manifest.json"));
  } catch {
    problems.push(
      `ClayJS ${version} as published has no offline index. Publish a ClayJS release that carries dist/offline-manifest.json, then re-vendor this host from it.`,
    );
    return { problems, files: 0 };
  }

  let publishedIndex;
  try {
    publishedIndex = JSON.parse(publishedBytes.toString("utf8"));
  } catch (err) {
    problems.push(`the published ClayJS ${version} offline index is not JSON (${err.message})`);
    return { problems, files: 0 };
  }

  if (!isDeepStrictEqual(publishedIndex, bundledIndex)) {
    problems.push(`the bundled ClayJS ${version} is not the published release (its offline index differs). Re-vendor from the published package.`);
  }

  const files = bundledIndex.files && typeof bundledIndex.files === "object" ? bundledIndex.files : {};
  const listed = Object.keys(files);
  for (const pub of listed) {
    if (typeof pub !== "string" || pub.split("/").some((segment) => !segment || segment === "." || segment === "..")) {
      problems.push(`the bundled ClayJS ${version} index lists ${JSON.stringify(pub)}, which is not a path inside the release`);
      continue;
    }
    let bytes;
    try {
      bytes = await readFile(path.join(bundledDir, ...pub.split("/")));
    } catch {
      problems.push(`the bundled ClayJS ${version} is missing ${pub}, which its offline index lists`);
      continue;
    }
    const digest = sha256(bytes);
    if (digest !== files[pub]?.sha256) {
      problems.push(`the bundled ClayJS ${version} file ${pub} is sha256 ${digest}, the offline index records ${files[pub]?.sha256}`);
    }
  }
  return { problems, files: listed.length };
}

async function main() {
  const argv = process.argv.slice(2);
  let packDir = null;
  let bundleRoot = ASSETS;
  for (let i = 0; i < argv.length; i += 1) {
    const arg = argv[i];
    const value = argv[i + 1];
    if (arg === "--pack-dir") {
      if (!value || value.startsWith("--")) fail(`${arg} needs a directory\n${USAGE}`);
      packDir = path.resolve(value);
      i += 1;
    } else if (arg === "--bundle-root") {
      if (!value || value.startsWith("--")) fail(`${arg} needs a directory\n${USAGE}`);
      bundleRoot = path.resolve(value);
      i += 1;
    } else {
      fail(`unknown argument ${arg}\n${USAGE}`);
    }
  }

  const lock = JSON.parse(await readFile(LOCK_FILE, "utf8"));
  if (!Array.isArray(lock?.versions) || !lock.versions.length) fail("offline-runtime.lock.json lists no versions");

  const tempDirs = [];
  const problems = [];
  const results = [];
  try {
    for (const version of lock.versions) {
      const packageRoot = await publishedPackage(version, packDir, tempDirs);
      const checked = await checkVersion(version, packageRoot, bundleRoot);
      problems.push(...checked.problems);
      results.push({ version, files: checked.files });
    }
  } catch (err) {
    problems.push(err.message);
  } finally {
    for (const dir of tempDirs) await rm(dir, { recursive: true, force: true });
  }

  if (problems.length) {
    for (const problem of problems) console.error(`${NAME}: ${problem}`);
    process.exitCode = 1;
    return;
  }
  for (const { version, files } of results) {
    console.log(`offline runtime: ${version} matches the published ClayJS release (${files} files)`);
  }
}

main().catch((err) => fail(err.message));

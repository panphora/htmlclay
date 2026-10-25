#!/usr/bin/env node
// Copies a locked ClayJS release out of a ClayJS package checkout into the Go binary's
// embedded asset tree, then proves the copy is a complete offline closure with ClayJS's
// own verifier. `--check` re-hashes the checked-in tree without touching anything, so CI
// can prove the vendored bytes still match the index and the lock.
import { execFileSync } from "node:child_process";
import { createHash } from "node:crypto";
import { mkdir, readFile, readdir, rename, rm, writeFile } from "node:fs/promises";
import path from "node:path";
import { fileURLToPath, pathToFileURL } from "node:url";

const ROOT = path.resolve(fileURLToPath(new URL("..", import.meta.url)));
const LOCK_FILE = path.join(ROOT, "offline-runtime.lock.json");
const ASSETS = path.join(ROOT, "internal", "clayruntime", "assets");
const VECTORS = path.join(ROOT, "internal", "clayruntime", "testdata", "vectors");
const VECTOR_FILES = ["urls.json", "html.json", "releases.json"];
const VERSION = /^\d+\.\d+\.\d+$/;

const sha256 = (bytes) => createHash("sha256").update(bytes).digest("hex");

function fail(message) {
  console.error(`vendor-clay-runtime: ${message}`);
  process.exit(1);
}

function releasePath(value, what) {
  if (typeof value !== "string" || value === "") {
    throw new Error(`${what} is not a non-empty string`);
  }
  if (value.includes("\\") || value.includes("\0")) {
    throw new Error(`${what} ${JSON.stringify(value)} contains a backslash or NUL`);
  }
  if (value.startsWith("/")) throw new Error(`${what} ${JSON.stringify(value)} is absolute`);
  if (value.split("/").some((segment) => segment === "" || segment === "." || segment === "..")) {
    throw new Error(`${what} ${JSON.stringify(value)} escapes the release`);
  }
  return value;
}

async function readLock() {
  const lock = JSON.parse(await readFile(LOCK_FILE, "utf8"));
  if (!lock || typeof lock !== "object" || Array.isArray(lock)) throw new Error("the lock is not an object");
  if (typeof lock.selected !== "string" || !VERSION.test(lock.selected)) {
    throw new Error(`lock.selected ${JSON.stringify(lock.selected)} is not a version`);
  }
  if (!Array.isArray(lock.versions) || !lock.versions.length) throw new Error("lock.versions is empty");
  for (const version of lock.versions) {
    if (typeof version !== "string" || !VERSION.test(version)) {
      throw new Error(`lock.versions names ${JSON.stringify(version)}, which is not a version`);
    }
  }
  if (!lock.versions.includes(lock.selected)) throw new Error("lock.selected is not listed in lock.versions");
  return lock;
}

const stable = (value) =>
  Array.isArray(value)
    ? `[${value.map(stable).join(",")}]`
    : value && typeof value === "object"
      ? `{${Object.keys(value).sort().map((key) => `${JSON.stringify(key)}:${stable(value[key])}`).join(",")}}`
      : JSON.stringify(value);

async function packageIndexes(packageRoots, lock) {
  const indexes = new Map();
  for (const packageRoot of packageRoots) {
    const root = path.resolve(packageRoot);
    const manifestBytes = await readFile(path.join(root, "dist", "offline-manifest.json"));
    const index = JSON.parse(manifestBytes.toString("utf8"));
    if (index?.schema !== 1) throw new Error(`${root}: the index schema is ${JSON.stringify(index?.schema)}, not 1`);
    if (typeof index.version !== "string" || !VERSION.test(index.version)) {
      throw new Error(`${root}: the index names version ${JSON.stringify(index.version)}`);
    }
    if (!lock.versions.includes(index.version)) {
      throw new Error(`${root}: publishes ${index.version}, which offline-runtime.lock.json does not lock`);
    }
    if (indexes.has(index.version)) throw new Error(`${root}: a second package root also publishes ${index.version}`);
    indexes.set(index.version, { root, index, manifestBytes });
  }
  for (const version of lock.versions) {
    if (!indexes.has(version)) throw new Error(`no --package-root publishes ${version}`);
  }
  return indexes;
}

async function vendor(packageRoots) {
  const lock = await readLock();
  const indexes = await packageIndexes(packageRoots, lock);
  const staging = `${ASSETS}.staging-${process.pid}`;
  await rm(staging, { recursive: true, force: true });
  try {
    await mkdir(staging, { recursive: true });
    for (const version of lock.versions) {
      const { root, index, manifestBytes } = indexes.get(version);
      const target = path.join(staging, version);
      await mkdir(target, { recursive: true });
      const files = index.files && typeof index.files === "object" ? index.files : {};
      const listed = Object.keys(files);
      if (!listed.length) throw new Error(`${version}: the index lists no files`);
      let bytesWritten = 0;
      for (const pub of listed) {
        releasePath(pub, `${version}: public path`);
        const entry = files[pub] ?? {};
        const source = releasePath(entry.source, `${version}: ${pub} source`);
        const bytes = await readFile(path.join(root, source));
        const digest = sha256(bytes);
        if (digest !== entry.sha256) {
          throw new Error(`${version}: ${pub} sha256 is ${digest}, the index records ${entry.sha256}`);
        }
        if (bytes.length !== entry.bytes) {
          throw new Error(`${version}: ${pub} is ${bytes.length} bytes, the index records ${entry.bytes}`);
        }
        const dest = path.join(target, ...pub.split("/"));
        await mkdir(path.dirname(dest), { recursive: true });
        await writeFile(dest, bytes);
        bytesWritten += bytes.length;
      }
      await writeFile(path.join(target, "offline-manifest.json"), manifestBytes);
      const { verifyOfflineClosure } = await import(
        pathToFileURL(path.join(root, "scripts", "verify-offline-closure.mjs")).href
      );
      const result = await verifyOfflineClosure({ publishedRoot: target });
      if (result.problems.length) {
        throw new Error(`${version}: the release is not an offline closure:\n${result.problems.join("\n")}`);
      }
      console.log(
        `verified ${version}: ${result.files} files, ${result.js} js, ${result.css} css, ${result.fonts} fonts, ` +
          `${result.staticEdges} static and ${result.dynamicEdges} dynamic edges, ${bytesWritten} bytes`,
      );
    }
    await writeFile(path.join(staging, "lock.json"), `${JSON.stringify(lock, null, 2)}\n`);
    await rm(ASSETS, { recursive: true, force: true });
    await rename(staging, ASSETS);
  } catch (err) {
    await rm(staging, { recursive: true, force: true });
    throw err;
  }

  const { root } = indexes.get(lock.selected);
  const commit = execFileSync("git", ["-C", root, "rev-parse", "HEAD"], { encoding: "utf8" }).trim();
  await mkdir(VECTORS, { recursive: true });
  for (const name of VECTOR_FILES) {
    const bytes = await readFile(path.join(root, "tests", "fixtures", "offline-runtime", name));
    await writeFile(path.join(VECTORS, name), bytes);
  }
  await writeFile(
    path.join(VECTORS, "SOURCE.md"),
    [
      "# Offline runtime vectors",
      "",
      `Copied from the ClayJS checkout by \`scripts/vendor-clay-runtime.mjs\`.`,
      "",
      `- ClayJS version: ${lock.selected}`,
      `- ClayJS commit: ${commit}`,
      `- Files: ${VECTOR_FILES.join(", ")}`,
      "",
      "Read-only: regenerate with the vendoring script, never by hand.",
      "",
    ].join("\n"),
  );
  console.log(`vendored clay runtime ${lock.versions.join(", ")} into internal/clayruntime/assets`);
}

async function walk(dir, prefix = "") {
  const found = [];
  for (const entry of await readdir(dir, { withFileTypes: true })) {
    const rel = prefix ? `${prefix}/${entry.name}` : entry.name;
    if (entry.isDirectory()) found.push(...(await walk(path.join(dir, entry.name), rel)));
    else found.push(rel);
  }
  return found;
}

async function check() {
  const lock = await readLock();
  let stored;
  try {
    stored = JSON.parse(await readFile(path.join(ASSETS, "lock.json"), "utf8"));
  } catch (err) {
    return `assets/lock.json: ${err.message}`;
  }
  if (stable(stored) !== stable(lock)) return "assets/lock.json does not match offline-runtime.lock.json";
  for (const version of lock.versions) {
    const dir = path.join(ASSETS, version);
    let index;
    try {
      index = JSON.parse(await readFile(path.join(dir, "offline-manifest.json"), "utf8"));
    } catch (err) {
      return `assets/${version}/offline-manifest.json: ${err.message}`;
    }
    const files = index.files && typeof index.files === "object" ? index.files : {};
    const listed = Object.keys(files);
    if (!listed.length) return `assets/${version}/offline-manifest.json lists no files`;
    for (const pub of listed) {
      let bytes;
      try {
        bytes = await readFile(path.join(dir, ...pub.split("/")));
      } catch (err) {
        return `assets/${version}/${pub}: ${err.message}`;
      }
      const digest = sha256(bytes);
      if (digest !== files[pub]?.sha256) {
        return `assets/${version}/${pub}: sha256 is ${digest}, the index records ${files[pub]?.sha256}`;
      }
      if (bytes.length !== files[pub]?.bytes) {
        return `assets/${version}/${pub}: is ${bytes.length} bytes, the index records ${files[pub]?.bytes}`;
      }
    }
    const known = new Set([...listed, "offline-manifest.json"]);
    for (const rel of await walk(dir)) {
      if (!known.has(rel)) return `assets/${version}/${rel}: the index does not list it`;
    }
    console.log(`checked ${version}: ${listed.length} files`);
  }
}

const USAGE = "usage: node scripts/vendor-clay-runtime.mjs --package-root <dir> [--package-root <dir> ...] | --check";

async function main() {
  const argv = process.argv.slice(2);
  const packageRoots = [];
  let checkOnly = false;
  for (let i = 0; i < argv.length; i += 1) {
    const arg = argv[i];
    if (arg === "--package-root") {
      const value = argv[i + 1];
      if (!value || value.startsWith("--")) fail(`${arg} needs a directory`);
      packageRoots.push(value);
      i += 1;
    } else if (arg === "--check") {
      checkOnly = true;
    } else {
      fail(`unknown argument ${arg}\n${USAGE}`);
    }
  }
  if (checkOnly) {
    if (packageRoots.length) fail(`--check takes no --package-root\n${USAGE}`);
    const problem = await check();
    if (problem) fail(problem);
    return;
  }
  if (!packageRoots.length) fail(USAGE);
  await vendor(packageRoots);
}

main().catch((err) => fail(err.message));

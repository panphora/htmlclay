#!/usr/bin/env node
// Proves the vendored ClayJS runtime is byte-identical to the published npm release of
// the locked version. The lock records a version only, so without this a build labeled
// 1.15.2 could ship commits past the real 1.15.2 and nothing would notice.
//
// It checks what the binary actually ships, not just what the index claims: the binary's
// own lock, the shape of both indexes, every file under the bundle root, and the bytes of
// the published package behind each listed file.
import { execFileSync } from "node:child_process";
import { createHash } from "node:crypto";
import { lstat, mkdtemp, readFile, readdir, rm } from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { isDeepStrictEqual } from "node:util";

const NAME = "check-clay-runtime-release";
const ROOT = path.resolve(fileURLToPath(new URL("..", import.meta.url)));
const LOCK_FILE = path.join(ROOT, "offline-runtime.lock.json");
const ASSETS = path.join(ROOT, "internal", "clayruntime", "assets");
const PACKAGE = "@panphora/clayjs";

// internal/clayruntime/embed.go reads the bundle's own lock at assets/lock.json and each
// release's index at assets/<version>/offline-manifest.json. Those are the only entries
// the binary can make sense of, so everything else under the bundle root is a stray.
const BUNDLE_FILES = new Set(["lock.json"]);
const RELEASE_FILES = new Set(["offline-manifest.json"]);

const sha256 = (bytes) => createHash("sha256").update(bytes).digest("hex");

const USAGE = `usage: node scripts/${NAME}.mjs [--pack-dir <dir>] [--bundle-root <dir>]`;

function fail(message) {
  console.error(`${NAME}: ${message}`);
  process.exit(1);
}

async function lstatSafe(target) {
  try {
    return await lstat(target);
  } catch {
    return null;
  }
}

// refuseSymlinks walks every component from root down to rel and reports a symbolic link on
// the way, so no release file can ever point outside the bundle. It answers with the last
// component's own lstat, or null when the path is missing or unsafe.
async function refuseSymlinks(problems, root, rel, label, display = rel) {
  const parts = rel.split("/").filter((part) => part.length);
  let current = root;
  let info = await lstatSafe(root);
  if (!info) {
    problems.push(`${label} ${display} has no bundle root at ${root}`);
    return null;
  }
  if (info.isSymbolicLink()) {
    problems.push(`the bundle root ${root} is a symbolic link, and a release cannot carry one`);
    return null;
  }
  for (const part of parts) {
    current = path.join(current, part);
    info = await lstatSafe(current);
    if (!info) {
      problems.push(`${label} ${display} is missing (${path.relative(root, current)} under ${root})`);
      return null;
    }
    if (info.isSymbolicLink()) {
      problems.push(`${label} ${display} is a symbolic link at ${path.relative(root, current)}, and a release cannot carry one`);
      return null;
    }
  }
  return info;
}

// safeRelative refuses the path shapes a package file may never take: empty, absolute,
// backslashed or climbing out with `..`.
function safeRelative(rel) {
  if (typeof rel !== "string" || rel === "") return false;
  if (rel.includes("\\")) return false;
  if (path.isAbsolute(rel) || rel.startsWith("/")) return false;
  return rel.split("/").every((part) => part !== "" && part !== "." && part !== "..");
}

// checkIndexShape reads the index format: schema 1, the version being checked, and a
// nonempty map of public path to file entry.
function checkIndexShape(problems, index, version, label) {
  if (!index || typeof index !== "object" || Array.isArray(index)) {
    problems.push(`${label} is not an object`);
    return false;
  }
  if (index.schema !== 1) {
    problems.push(`${label} has schema ${JSON.stringify(index.schema)}, want 1`);
    return false;
  }
  if (index.version !== version) {
    problems.push(`${label} names version ${JSON.stringify(index.version)}, want ${version}`);
    return false;
  }
  if (!index.files || typeof index.files !== "object" || Array.isArray(index.files)) {
    problems.push(`${label} has no files object`);
    return false;
  }
  if (Object.keys(index.files).length === 0) {
    problems.push(`${label} lists no files`);
    return false;
  }
  return true;
}

// checkBundleLock holds the binary's own lock to the repository lock. embed.go reads
// assets/lock.json and panics unless `selected` is one of `versions`, so a bundle whose
// lock disagrees with the repository ships a runtime nobody asked for.
async function checkBundleLock(problems, bundleRoot, lock) {
  const target = path.join(bundleRoot, "lock.json");
  const label = `the bundled ClayJS lock at ${target}`;
  const info = await refuseSymlinks(problems, bundleRoot, "lock.json", "the bundled ClayJS lock");
  if (!info) return;
  if (!info.isFile()) {
    problems.push(`${label} is not a regular file`);
    return;
  }
  let bundled;
  try {
    bundled = JSON.parse(await readFile(target, "utf8"));
  } catch (err) {
    problems.push(`${label} is not readable JSON (${err.message})`);
    return;
  }
  if (bundled?.selected !== lock.selected || !isDeepStrictEqual(bundled?.versions, lock.versions)) {
    problems.push(
      `${label} ships versions ${JSON.stringify(bundled?.versions)} selected ${JSON.stringify(bundled?.selected)}, ` +
        `offline-runtime.lock.json says versions ${JSON.stringify(lock.versions)} selected ${JSON.stringify(lock.selected)}`,
    );
  }
  const versions = Array.isArray(bundled?.versions) ? bundled.versions : [];
  if (!versions.includes(bundled?.selected)) {
    problems.push(`${label} selects ${JSON.stringify(bundled?.selected)}, which its versions ${JSON.stringify(bundled?.versions)} do not list`);
  }
}

// checkBundleRoot allows only the lock and the release directories the lock names directly
// under the bundle root.
async function checkBundleRoot(problems, bundleRoot, lock) {
  let entries;
  try {
    entries = await readdir(bundleRoot, { withFileTypes: true });
  } catch (err) {
    problems.push(`the bundle root ${bundleRoot} cannot be read (${err.message})`);
    return;
  }
  for (const entry of entries) {
    const info = await lstatSafe(path.join(bundleRoot, entry.name));
    if (!info) continue;
    if (info.isSymbolicLink()) {
      problems.push(`the bundle root entry ${entry.name} is a symbolic link, and a release cannot carry one`);
      continue;
    }
    if (info.isDirectory()) {
      if (!lock.versions.includes(entry.name)) {
        problems.push(`the bundle root carries the directory ${entry.name}, which offline-runtime.lock.json does not list`);
      }
      continue;
    }
    if (!info.isFile()) {
      problems.push(`the bundle root entry ${entry.name} is not a regular file`);
      continue;
    }
    if (!BUNDLE_FILES.has(entry.name)) {
      problems.push(`the bundle root carries the extra file ${entry.name}, which neither the lock nor a release index accounts for`);
    }
  }
}

// checkReleaseTree walks <bundle-root>/<version> itself: every regular file must be listed
// in the release index, and a symbolic link or a special file refuses.
async function checkReleaseTree(problems, bundleRoot, version, files) {
  const label = `the bundled ClayJS ${version} release directory`;
  const info = await refuseSymlinks(problems, bundleRoot, version, label);
  if (!info) return;
  if (!info.isDirectory()) {
    problems.push(`${label} is not a directory`);
    return;
  }
  const listed = new Set(Object.keys(files));
  const walk = async (rel) => {
    let entries;
    try {
      entries = await readdir(path.join(bundleRoot, ...rel.split("/")), { withFileTypes: true });
    } catch (err) {
      problems.push(`the bundled ClayJS ${version} directory ${rel.slice(version.length + 1) || "."} cannot be read (${err.message})`);
      return;
    }
    for (const entry of entries) {
      const childRel = `${rel}/${entry.name}`;
      const inner = childRel.slice(version.length + 1);
      const child = await lstatSafe(path.join(bundleRoot, ...childRel.split("/")));
      if (!child) continue;
      if (child.isSymbolicLink()) {
        problems.push(`the bundled ClayJS ${version} carries the symbolic link ${inner}, and a release cannot carry one`);
        continue;
      }
      if (child.isDirectory()) {
        await walk(childRel);
        continue;
      }
      if (!child.isFile()) {
        problems.push(`the bundled ClayJS ${version} file ${inner} is not a regular file`);
        continue;
      }
      if (RELEASE_FILES.has(inner)) continue;
      if (!listed.has(inner)) {
        problems.push(`the bundled ClayJS ${version} carries the extra file ${inner}, which its offline index does not list`);
      }
    }
  };
  await walk(version);
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
  if (!checkIndexShape(problems, bundledIndex, version, `the bundled ClayJS ${version} offline index`)) {
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
  if (!checkIndexShape(problems, publishedIndex, version, `the published ClayJS ${version} offline index`)) {
    return { problems, files: 0 };
  }

  if (!isDeepStrictEqual(publishedIndex, bundledIndex)) {
    problems.push(`the bundled ClayJS ${version} is not the published release (its offline index differs). Re-vendor from the published package.`);
  }

  await checkReleaseTree(problems, bundleRoot, version, bundledIndex.files);

  const files = bundledIndex.files;
  const listed = Object.keys(files);
  const bundledHashes = new Map();
  for (const pub of listed) {
    if (!safeRelative(pub)) {
      problems.push(`the bundled ClayJS ${version} index lists ${JSON.stringify(pub)}, which is not a path inside the release`);
      continue;
    }
    const info = await refuseSymlinks(
      problems,
      bundleRoot,
      `${version}/${pub}`,
      `the bundled ClayJS ${version} file`,
      pub,
    );
    if (!info) continue;
    if (!info.isFile()) {
      problems.push(`the bundled ClayJS ${version} file ${pub} is not a regular file`);
      continue;
    }
    let bytes;
    try {
      bytes = await readFile(path.join(bundledDir, ...pub.split("/")));
    } catch (err) {
      problems.push(`the bundled ClayJS ${version} file ${pub} cannot be read (${err.message})`);
      continue;
    }
    const digest = sha256(bytes);
    bundledHashes.set(pub, digest);
    if (digest !== files[pub]?.sha256) {
      problems.push(`the bundled ClayJS ${version} file ${pub} is sha256 ${digest}, the offline index records ${files[pub]?.sha256}`);
    }
  }

  for (const pub of listed) {
    const entry = files[pub];
    if (!safeRelative(entry?.source)) {
      problems.push(`the published ClayJS ${version} index lists ${pub} with source ${JSON.stringify(entry?.source)}, which is not a path inside the package`);
      continue;
    }
    const target = path.join(packageRoot, ...entry.source.split("/"));
    if (!target.startsWith(packageRoot + path.sep)) {
      problems.push(`the published ClayJS ${version} index lists ${pub} with source ${JSON.stringify(entry.source)}, which is not a path inside the package`);
      continue;
    }
    const info = await lstatSafe(target);
    if (!info) {
      problems.push(`the published ClayJS ${version} has no file at ${entry.source} (${pub}), which its offline index lists`);
      continue;
    }
    if (!info.isFile()) {
      problems.push(`the published ClayJS ${version} file ${entry.source} is not a regular file`);
      continue;
    }
    let bytes;
    try {
      bytes = await readFile(target);
    } catch (err) {
      problems.push(`the published ClayJS ${version} file ${entry.source} cannot be read (${err.message})`);
      continue;
    }
    const digest = sha256(bytes);
    if (digest !== entry.sha256) {
      problems.push(`the published ClayJS ${version} file ${entry.source} is sha256 ${digest}, the offline index records ${entry.sha256}`);
    }
    const bundledDigest = bundledHashes.get(pub);
    if (bundledDigest && bundledDigest !== digest) {
      problems.push(`the published ClayJS ${version} file ${entry.source} is sha256 ${digest}, the bundled ${pub} is ${bundledDigest}`);
    }
    if (typeof entry.bytes === "number" && bytes.length !== entry.bytes) {
      problems.push(`the published ClayJS ${version} file ${entry.source} is ${bytes.length} bytes, the offline index records ${entry.bytes}`);
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
    await checkBundleLock(problems, bundleRoot, lock);
    await checkBundleRoot(problems, bundleRoot, lock);
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

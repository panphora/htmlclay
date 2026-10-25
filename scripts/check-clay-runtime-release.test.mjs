import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import { createHash } from "node:crypto";
import { mkdir, mkdtemp, readFile, rm, symlink, writeFile } from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import { test } from "node:test";
import { fileURLToPath } from "node:url";

const ROOT = path.resolve(fileURLToPath(new URL("..", import.meta.url)));
const SCRIPT = path.join(ROOT, "scripts", "check-clay-runtime-release.mjs");
const sha256 = (bytes) => createHash("sha256").update(bytes).digest("hex");

async function write(root, rel, bytes) {
  const target = path.join(root, ...rel.split("/"));
  await mkdir(path.dirname(target), { recursive: true });
  await writeFile(target, bytes);
}

async function fixture(
  t,
  {
    publishedIndex = true,
    indexMutator = null,
    bundledIndexMutator = null,
    bundledMutator = null,
    bundleLockMutator = null,
    extraFiles = [],
    symlinkFile = null,
    publishedFileMutator = null,
    missingPublishedFile = null,
  } = {},
) {
  const lock = JSON.parse(await readFile(path.join(ROOT, "offline-runtime.lock.json"), "utf8"));
  const version = lock.versions[0];
  const root = await mkdtemp(path.join(os.tmpdir(), "clay-runtime-release-test-"));
  t.after(() => rm(root, { recursive: true, force: true }));

  const bundleRoot = path.join(root, "assets");
  const packDir = path.join(root, "pack");
  const files = { "dist/a.js": "alpha\n", "dist/b.css": "beta\n" };
  const index = {
    schema: 1,
    version,
    files: Object.fromEntries(
      Object.entries(files).map(([pub, body]) => [
        pub,
        { source: pub, sha256: sha256(Buffer.from(body)), bytes: Buffer.byteLength(body) },
      ]),
    ),
  };

  const bundledLock = bundleLockMutator ? bundleLockMutator(structuredClone(lock)) : lock;
  await write(bundleRoot, "lock.json", `${JSON.stringify(bundledLock, null, 2)}\n`);

  for (const [pub, body] of Object.entries(files)) {
    const bytes = bundledMutator ? bundledMutator(pub, body) : body;
    await write(bundleRoot, `${version}/${pub}`, bytes);
  }
  const bundledIndex = bundledIndexMutator ? bundledIndexMutator(structuredClone(index)) : index;
  await write(bundleRoot, `${version}/offline-manifest.json`, `${JSON.stringify(bundledIndex, null, 2)}\n`);

  for (const extra of extraFiles) {
    await write(bundleRoot, `${version}/${extra.rel}`, extra.bytes);
  }

  if (symlinkFile) {
    const target = path.join(bundleRoot, version, ...symlinkFile.split("/"));
    await rm(target, { force: true });
    await symlink("b.css", target);
  }

  if (publishedIndex) {
    const published = indexMutator ? indexMutator(structuredClone(index)) : index;
    await write(packDir, `${version}/package/dist/offline-manifest.json`, `${JSON.stringify(published, null, 2)}\n`);
    for (const [pub, body] of Object.entries(files)) {
      if (pub === missingPublishedFile) continue;
      const bytes = publishedFileMutator ? publishedFileMutator(pub, body) : body;
      await write(packDir, `${version}/package/${pub}`, bytes);
    }
  } else {
    await write(packDir, `${version}/package/dist/README.md`, "no index here\n");
  }
  return { version, bundleRoot, packDir };
}

function run({ packDir, bundleRoot }) {
  return spawnSync(process.execPath, [SCRIPT, "--pack-dir", packDir, "--bundle-root", bundleRoot], {
    cwd: ROOT,
    encoding: "utf8",
  });
}

test("passes when the bundled tree matches the published release", async (t) => {
  const f = await fixture(t);
  const result = run(f);
  assert.equal(result.status, 0, result.stderr);
  assert.match(result.stdout, new RegExp(`offline runtime: ${f.version} matches the published ClayJS release \\(2 files\\)`));
});

test("fails when the bundled lock selects a release the repository lock does not", async (t) => {
  const f = await fixture(t, {
    bundleLockMutator: (lock) => ({ ...lock, selected: "9.9.9" }),
  });
  const result = run(f);
  assert.equal(result.status, 1);
  assert.match(result.stderr, /^check-clay-runtime-release: /m);
  assert.match(result.stderr, /selects "9\.9\.9", which its versions/);
});

test("fails when the published package has no offline index", async (t) => {
  const f = await fixture(t, { publishedIndex: false });
  const result = run(f);
  assert.equal(result.status, 1);
  assert.match(result.stderr, /as published has no offline index/);
  assert.match(result.stderr, /^check-clay-runtime-release: /m);
});

test("fails when the published index differs from the bundled index", async (t) => {
  const f = await fixture(t, {
    indexMutator: (index) => {
      index.files["dist/a.js"].sha256 = "0".repeat(64);
      return index;
    },
  });
  const result = run(f);
  assert.equal(result.status, 1);
  assert.match(result.stderr, /is not the published release \(its offline index differs\)/);
});

test("fails when a bundled file's bytes differ from the index", async (t) => {
  const f = await fixture(t, {
    bundledMutator: (pub, body) => (pub === "dist/a.js" ? "tampered\n" : body),
  });
  const result = run(f);
  assert.equal(result.status, 1);
  assert.match(result.stderr, /file dist\/a\.js is sha256/);
});

test("fails when the bundled index lists no files", async (t) => {
  const f = await fixture(t, {
    bundledIndexMutator: (index) => ({ ...index, files: {} }),
  });
  const result = run(f);
  assert.equal(result.status, 1);
  assert.match(result.stderr, /offline index lists no files/);
});

test("fails when the bundled index has the wrong schema", async (t) => {
  const f = await fixture(t, {
    bundledIndexMutator: (index) => ({ ...index, schema: 999 }),
  });
  const result = run(f);
  assert.equal(result.status, 1);
  assert.match(result.stderr, /offline index has schema 999, want 1/);
});

test("fails when the bundled tree carries a file the index does not list", async (t) => {
  const f = await fixture(t, {
    extraFiles: [{ rel: "dist/extra.txt", bytes: "extra\n" }],
  });
  const result = run(f);
  assert.equal(result.status, 1);
  assert.match(result.stderr, /carries the extra file dist\/extra\.txt, which its offline index does not list/);
});

test("fails when a listed bundled file is a symbolic link", async (t) => {
  const f = await fixture(t, { symlinkFile: "dist/a.js" });
  const result = run(f);
  assert.equal(result.status, 1);
  assert.match(result.stderr, /file dist\/a\.js is a symbolic link/);
});

test("fails when a published asset's bytes differ from the unchanged index", async (t) => {
  const f = await fixture(t, {
    publishedFileMutator: (pub, body) => (pub === "dist/a.js" ? "other\n" : body),
  });
  const result = run(f);
  assert.equal(result.status, 1);
  assert.match(result.stderr, /the published ClayJS \S+ file dist\/a\.js is sha256/);
});

test("fails when a published asset the index lists is missing", async (t) => {
  const f = await fixture(t, { missingPublishedFile: "dist/a.js" });
  const result = run(f);
  assert.equal(result.status, 1);
  assert.match(result.stderr, /has no file at dist\/a\.js \(dist\/a\.js\), which its offline index lists/);
});

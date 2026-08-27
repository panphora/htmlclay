#!/usr/bin/env node
// Stamps the release version into the website pages.
//
// Anchored on data attributes, so a new spot on a page needs no change here:
//   data-version   the element's text becomes v<version>
//   data-mac-dmg   href and text become the macOS dmg URL and filename
//   data-sha256    the element's text becomes that artifact's published hash
//
// Only the macOS download carries a version in its filename; the Windows and
// Linux artifacts have stable names and need no stamping. Rewrites from
// whatever is currently on the page, so it is self-correcting after drift.

import { readFileSync, writeFileSync } from 'node:fs';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';

const ROOT = join(dirname(fileURLToPath(import.meta.url)), '..');
// Only pages that actually carry a version or a download link. A page listed here
// with neither is a hard error, which is what catches a stamp slot that quietly
// disappeared; features.html is deliberately absent because it carries no version.
const PAGES = ['index.html'].map((name) => join(ROOT, 'website', name));
const DOWNLOAD_BASE = 'https://download.htmlclay.com/';

const version =
  process.argv[2] ||
  readFileSync(join(ROOT, 'cmd/htmlclay/main.go'), 'utf8').match(/var version = "(.*)"/)[1];

const dmgName = `HTMLClay-${version}-universal.dmg`;

// Hashes only exist once CI has built and uploaded the artifacts, so the checksum
// file is optional: a run without it stamps versions and leaves the hash slots empty,
// which the page hides. When it IS given, a slot it cannot fill is a hard error
// rather than a blank, because a hash that looks checkable and is not is worse than
// no hash at all.
const checksumsPath = process.argv[3];
const hashes = checksumsPath
  ? new Map(
      readFileSync(checksumsPath, 'utf8')
        .trim()
        .split('\n')
        .filter(Boolean)
        .map((line) => {
          const match = line.match(/^([0-9a-f]{64})\s+(.+)$/);
          if (!match) throw new Error(`Invalid SHA256SUMS line: ${line}`);
          return [match[2], match[1]];
        })
    )
  : null;
let hashNodes = 0;

for (const page of PAGES) {
  let html = readFileSync(page, 'utf8');
  let stamped = 0;

  html = html.replace(
    /(<[^>]*\sdata-version(?=[\s>])[^>]*>)[^<]*(<\/)/g,
    (_, open, close) => {
      stamped++;
      return `${open}v${version}${close}`;
    }
  );

  html = html.replace(
    /(<a[^>]*\sdata-mac-dmg(?=[\s>])[^>]*>)[^<]*(<\/a>)/g,
    (_, open, close) => {
      stamped++;
      const tag = open.replace(/href="[^"]*"/, `href="${DOWNLOAD_BASE}${dmgName}"`);
      return `${tag}${dmgName}${close}`;
    }
  );

  html = html.replace(
    /data-sha256="HTMLClay-[^"]+-universal\.dmg"/g,
    `data-sha256="${dmgName}"`
  );

  if (hashes) {
    html = html.replace(
      /(<[^>]*\sdata-sha256="([^"]+)"[^>]*>)[^<]*(<\/[a-z]+>)/g,
      (_, open, filename, close) => {
        const hash = hashes.get(filename);
        if (!hash) throw new Error(`SHA256SUMS has no hash for ${filename}`);
        hashNodes++;
        return `${open}${hash}${close}`;
      }
    );
  }

  if (stamped === 0) {
    console.error(`stamp-website: found no data-version or data-mac-dmg elements in ${page}`);
    process.exit(1);
  }

  writeFileSync(page, html);
  console.log(`Stamped ${stamped} element(s) in ${page.split('/').pop()} with v${version}`);
}

if (hashes && hashNodes === 0) {
  console.error('stamp-website: a checksum file was given but no data-sha256 elements were found');
  process.exit(1);
}

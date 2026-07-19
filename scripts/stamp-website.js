#!/usr/bin/env node
// Stamps the release version into website/index.html.
//
// Anchored on data attributes, so a new spot on the page needs no change here:
//   data-version   the element's text becomes v<version>
//   data-mac-dmg   href and text become the macOS dmg URL and filename
//
// Only the macOS download carries a version in its filename; the Windows and
// Linux artifacts have stable names and need no stamping. Rewrites from
// whatever is currently on the page, so it is self-correcting after drift.

import { readFileSync, writeFileSync } from 'node:fs';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';

const ROOT = join(dirname(fileURLToPath(import.meta.url)), '..');
const PAGE = join(ROOT, 'website', 'index.html');
const DOWNLOAD_BASE = 'https://download.htmlclay.com/';

const version =
  process.argv[2] ||
  readFileSync(join(ROOT, 'main.go'), 'utf8').match(/var version = "(.*)"/)[1];

const dmgName = `HTMLClay-${version}-universal.dmg`;

let html = readFileSync(PAGE, 'utf8');
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

if (stamped === 0) {
  console.error('stamp-website: found no data-version or data-mac-dmg elements');
  process.exit(1);
}

writeFileSync(PAGE, html);
console.log(`Stamped ${stamped} element(s) with v${version}`);

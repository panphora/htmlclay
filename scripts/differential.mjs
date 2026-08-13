// Cross-language differential: random (document, rules) pairs through cheerio, then through the Go
// port, comparing the answers.
//
// This is a DEVELOPMENT harness, not a CI check, and that is deliberate. htmlclay vendors no
// JavaScript engine, so a Go test that skipped when node was missing would be a parity check that
// silently does not run — the most expensive kind of green. CI runs the vendored conformance corpus
// and the native Go fuzz seeds; this script is what a person runs when changing the engine, and
// every failure it finds should be persisted as a deterministic corpus case.
//
//   node scripts/differential.mjs [--cases 500] [--seed 1]
//
// The contract it enforces has three cases, and only two of them are failures:
//
//   both answer          the node sets must be IDENTICAL           -> mismatch is a BUG
//   cheerio errors, Go answers                                     -> a BUG, the port invented data
//   Go refuses, cheerio answers                                    -> a deliberate divergence, counted
//
// The third is the gate doing its job: it refuses constructs the two engines read differently. The
// count is reported so a change that widens it is visible rather than silent.

import { createRequire } from "node:module";
import { execFileSync } from "node:child_process";
import { mkdtempSync, writeFileSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { resolve, join } from "node:path";

const reference = process.env.HYPER_HTML_API ?? "../hyper-html-api";
const require = createRequire(resolve(reference, "package.json"));
const cheerio = require("cheerio");

const arg = (name, fallback) => {
  const i = process.argv.indexOf(`--${name}`);
  return i >= 0 && process.argv[i + 1] ? Number(process.argv[i + 1]) : fallback;
};
const CASES = arg("cases", 500);

// A small deterministic PRNG, so a failing run is reproducible from its seed alone.
let state = arg("seed", 1) >>> 0 || 1;
const rand = () => {
  state ^= state << 13; state >>>= 0;
  state ^= state >> 17;
  state ^= state << 5; state >>>= 0;
  return state / 0x100000000;
};
const pick = (xs) => xs[Math.floor(rand() * xs.length)];
const times = (n, fn) => Array.from({ length: n }, (_, i) => fn(i));

const TAGS = ["div", "p", "li", "span", "a", "b", "em", "input", "option", "h1"];
const CLASSES = ["row", "note", "", "row note", "Row"];
const TEXTS = ["alpha", "Beta", "gamma beta", "", " padded ", "a,b", "<x>", "&amp;", "ünïcode"];
const ATTRS = ["href", "rel", "data-x", "type", "hreflang", "dir", "id"];
const VALUES = ["/docs", "/Docs", "nofollow", "NOFOLLOW", "abc", "ABC", "", "checkbox", "en"];

function randomDoc() {
  const body = times(3 + Math.floor(rand() * 6), (i) => {
    const tag = pick(TAGS);
    const cls = pick(CLASSES);
    const attrs = times(Math.floor(rand() * 3), () => `${pick(ATTRS)}="${pick(VALUES)}"`).join(" ");
    return `<${tag} id="n${i}"${cls ? ` class="${cls}"` : ""}${attrs ? " " + attrs : ""}>${pick(TEXTS)}</${tag}>`;
  }).join("");
  return `<!doctype html><html><head><title>t</title></head><body><ul id="list">${body}</ul></body></html>`;
}

// Selector fragments span both what the gate admits and what it refuses on purpose, so the run
// exercises the refusal path as well as the parity path.
const SELECTORS = [
  "*", "li", ".row", "#list li", "ul > li", "li.row", "[href]", "[rel=nofollow]", "[rel!=nofollow]",
  "[data-x=abc i]", "[type=checkbox]", "[hreflang|=en]", "[rel^=nofol]", "[rel*=OFOL]",
  "li:first", "li:last", "li:eq(1)", "li:eq(-1)", "li:nth(0)", "li:contains(beta)",
  "li:contains( beta )", "li:not(.row)", "li:has(b)", "li:nth-child(2)", "li:first-of-type",
  ":checked", "option:checked", ":root", "li, .note", "li:first, a:first",
  // Constructs the gate refuses, each measured as a divergence rather than a bug.
  "li:empty", ":is(li)", "li:gt(0)", "li/*x*/span", "li,", "[rel!=\"\"]", "li:first span",
  "[data-x=ABC s]", "li:matches(.row)", "[id%b]",
];
// @type and @readOnly are deliberately ABSENT. cheerio reads them off the DOM node rather than the
// attribute, so @type answers "tag" (domhandler's node type) on every element and @readOnly is
// always false. htmlclay ships both FIXED, which makes them permanent, known mismatches — they are
// pinned by the props-type-is-node-type and props-readonly-always-false corpus cases, and generating
// them here would only bury real findings under noise the corpus already covers.
const PROPS = ["", "@href", "@id", "@class", "@outerHTML", "@textContent", "@rel", "@data-x"];

function randomRules() {
  const key = (i) => `k${i}`;
  const shape = rand();
  if (shape < 0.15) {
    return { [key(0)]: [pick(SELECTORS), { t: ".", a: pick(PROPS) || "@id" }] };
  }
  const out = {};
  for (let i = 0; i < 1 + Math.floor(rand() * 3); i++) {
    const sel = pick(SELECTORS);
    const prop = pick(PROPS);
    out[key(i)] = rand() < 0.3 ? `${sel}[]` : prop ? `${sel}${prop}` : sel;
  }
  return out;
}

const { extract } = await import(resolve(reference, "src/engine/index.js"));
const cheerioAdapter = (await import(resolve(reference, "src/adapters/cheerio.js"))).default;

const cases = [];
for (let i = 0; i < CASES; i++) {
  const html = randomDoc();
  const rules = randomRules();
  let answer = null;
  let failed = false;
  try {
    answer = extract(cheerioAdapter, cheerio.load(html).root(), rules);
  } catch {
    failed = true;
  }
  cases.push({ html, rules, cheerioOK: !failed, cheerio: failed ? null : answer });
}

const dir = mkdtempSync(join(tmpdir(), "htmlclay-diff-"));
const casesPath = join(dir, "cases.json");
writeFileSync(casesPath, JSON.stringify(cases));

let report;
try {
  const out = execFileSync(
    "go",
    ["test", "./dataapi/", "-run", "TestDifferential", "-count=1", "-v"],
    { env: { ...process.env, HTMLCLAY_DIFFERENTIAL: casesPath }, encoding: "utf8" },
  );
  report = out;
} catch (e) {
  report = (e.stdout || "") + (e.stderr || "");
  process.exitCode = 1;
}
rmSync(dir, { recursive: true, force: true });

console.log(report.split("\n").filter((l) => /DIFFERENTIAL|FAIL|ok |PASS|rules:|html:|  go:|cheerio:/.test(l)).join("\n"));

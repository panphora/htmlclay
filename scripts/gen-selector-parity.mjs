// Records cheerio's answer for every selector construct the gate has an opinion about, so the Go
// side is checked against the reference engine rather than against my expectations.
//
// The fixture deliberately contains NO <template>, no script[data-rules-name] and no [cms-template]:
// those three are decided elsewhere (shadow tree, rules-tag filter, seed filter) and have their own
// tests. Mixing them in here would make a gate failure look like an adapter failure.
// Run via `make sync-selector-parity`, which points HYPER_HTML_API at the reference checkout. The
// import is resolved from that path rather than from this file, so the recorded answers come from
// the exact cheerio the reference engine runs on.
import { writeFileSync } from "node:fs";
import { createRequire } from "node:module";
import { resolve } from "node:path";

const reference = process.env.HYPER_HTML_API ?? "../hyper-html-api";
const require = createRequire(resolve(reference, "package.json"));
const cheerio = require("cheerio");

const FIXTURE = [
  '<!doctype html><html><head><title id="ti">t</title></head><body>',
  '<ul id="list">',
  '<li id="l1" class="row">alpha</li>',
  '<li id="l2" class="row">Beta</li>',
  '<li id="l3" class="row">gamma beta</li>',
  '</ul>',
  '<ol id="mixed"><li id="m1">one</li><p id="mp">p</p><li id="m2">two</li></ol>',
  '<select id="sel"><option id="o1">a</option><option id="o2">b</option></select>',
  '<select id="sel2"><option id="o3">a</option><option id="o4" selected>b</option></select>',
  '<select id="sel3" multiple><option id="o5">a</option></select>',
  '<select id="sel4"><optgroup id="og"><option id="o6">a</option></optgroup></select>',
  '<input id="i1" checked><input id="i2" type="CHECKBOX" checked><input id="i3" type="radio">',
  '<a id="a1" href="/Docs" rel="NOFOLLOW" hreflang="EN">doc</a>',
  '<a id="a2" href="/docs" rel="nofollow" hreflang="en">doc</a>',
  '<div id="d1" dir="LTR" data-x="ABC"><b id="b1">deep</b></div>',
  '<p id="p1" class="note">has <em id="e1">emphasis</em> inside</p>',
  '<div id="solo"><span id="s1">only</span></div>',
  '</body></html>',
].join("\n");

// Constructs the gate ALLOWS. Go must return exactly what cheerio returns.
const PARITY = [
  // Measured one rune at a time: identifier characters to cheerio AND cascadia, so refusing them
  // would be a false alarm. They belong here precisely because they look like whitespace.
  "li\u2028span", "li\ufeffspan", "li\u3000span", "li\u2000span", "li\u200bspan",

  ".row:first", ".row:last", ".row:eq(1)", ".row:eq(-1)", ".row:eq(9)", ".row:eq(0)",
  "li:first", "#list li:last", "li :first", "#solo :first", "#mixed li:last",
  "li:contains(beta)", "li:contains(Beta)", 'li:contains("beta")', "li:contains(a)",
  "p:contains(emphasis)", "li:CONTAINS(beta)", "li:\\63 ontains(beta)",
  ":checked", "option:checked", "input:checked", "#sel option:checked",
  "#sel2 option:checked", "#sel3 option:checked", "#sel4 option:checked", "select:checked",
  "[rel=nofollow]", "[rel=NOFOLLOW]", "[dir=ltr]", "[dir=LTR]", "[hreflang=en]",
  "[type=checkbox]", "[type=CHECKBOX]",
  "[href='/docs']", "[href='/DOCS']", "[data-x=abc]", "[data-x=ABC]",
  "[data-x=abc i]", "[href='/docs' i]",
  "[rel^=NOFOL]", "[rel$=LLOW]", "[rel*=OFOL]", "[rel~=NOFOLLOW]", "[hreflang|=EN]",
  "[rel^=nofol]", "[rel$=llow]", "[rel*=ofol]", "[rel~=nofollow]", "[hreflang|=en]",
  "[rel]", "[data-x]", "[href='']",
  "li:nth-child(2)", "li:nth-child(odd)", "li:nth-child(2n+1)", "li:nth-last-child(1)",
  "li:first-child", "li:last-child", "li:first-of-type", "li:last-of-type",
  "li:only-child", "li:only-of-type", "li:nth-of-type(1)", "li:nth-last-of-type(1)",
  "#mixed li:first-of-type", "#mixed li:last-of-type", "#mixed p:only-of-type",
  "#solo span:only-child", ":root",
  "#list:has(li)", "li:not(.row)", "div:has(b)",
  "ul li", "ul > li", "#l1 + li", "#l1 ~ li", "*", "li.row", "li#l1",
  "li:first, a:first", ".row:first, .note", "#l1, #l2", "#l2, #l1",
  "li:contains(beta), li:contains(alpha)",
  // != : both engines accept it, and the folding names must fold through it too.
  "[rel!=nofollow]", "[rel!=NOFOLLOW]", "[type!=checkbox]", "[type!=CHECKBOX]",
  "[data-x!=abc]", "[data-x!=ABC]", "input[type!=text]", "[href!='/docs']",
  // :eq boundaries — cheerio's rule is abs(idx) < len, which jQuery's idx += len gets wrong by one.
  "li:eq(-1)", "li:eq(-2)", "li:eq(-3)", "li:eq(-4)", "#solo span:eq(-1)", "#solo span:eq(0)",
  // :eq parses with parseInt, not strconv.Atoi.
  "li:eq(1.9)", "li:eq(1abc)", "li:eq()", "li:eq(+1)",
  // :nth is a live cheerio-select positional and an exact alias of :eq.
  "li:nth(0)", "li:nth(1)", "li:nth(-1)",
  // :contains does NOT trim its argument.
  "li:contains( beta )", "li:contains( beta)", "li:contains(beta )",
  'li:contains(" beta")', "p:contains( )",
  // pseudo arity.
  "h1:contains()", "li:first",
];

// Constructs the gate REJECTS. cheerio's answer is recorded to document exactly what a caller
// gives up, so the divergence is a stated cost rather than an unknown.
const REJECTED = [
  "li:empty", ":is(li)", ":where(li)", "li:gt(0)", "li:lt(2)", "li:even", "li:odd",
  "input:enabled", "input:disabled", "option:selected", "li:parent", ":header",
  "li:matches(.row)", "li:containsOwn(alpha)", "div:haschild(b)", "p:lang(en)",
  ":scope li", "li::before", "li:focus", "li:target", ":input", "a:link",
  "a:visited", "a:hover", "a:active", "li:nth-childx(2)",
  "li:\\6d atches(.row)", "div:\\6D ATCHES(b)",
  ".row:first:first", "li:first span", ".row:eq(0) b", "li:contains(a) b", ":checked + b",
  "[href*='']", "[href^='']", "[href$='']",
  "li:has(> b)", "div:has(> b)",
  // A peelable buried in an argument: cascadia has :contains but folds case, so it cannot stand in.
  "li:not(:contains(a))", "li:has(:contains(a))", "li:not(:first)", "li:not(:checked)",
  // Empty groups. A blank selector is [] in cheerio (handled by Find, not the gate); a comma-made
  // empty group throws there and is refused here.
  "li,", ",li", "li,,li", "li, ", " ,li",
  // CSS comments: cascadia strips them as whitespace, css-what has no comment syntax.
  "li/*x*/span", "li /*x*/ span", "li/*[*/:matches(x)", "#list[id=list/*]*/]", "li/*'*/span",
  // != with an empty value diverges the same way ^= $= *= do.
  '[rel!=""]', '[rel~=""]', '[rel^=""]', '[rel$=""]', '[rel*=""]',
  // A positional that is not terminal cannot be expressed as a post-filter.
  "li:first span", "li:first :contains(a)", "li:contains(a) :first", "li:first > span",
  ".row:first:first", "li:first:last", "li:nth(0):first",
  // The s flag: cheerio honours it, cascadia takes only i.
  "[data-x=ABC s]", "[type=checkbox s]",
  // An operator the scanner cannot name. The skip used to land on ']', so `[a%b]` reached the gate
  // looking like the presence check `[a]`; cascadia rejects these, which is exactly the assumption
  // the phase-4 review disproved, so the scanner refuses them itself now.
  "[id%b]", "[id?b]", "[id@b]", "[id%=b]", "[id$b]",
  // The three characters the two engines disagree about: separator to css-what, identifier
  // character to cascadia. Found by FuzzScanSelector's closure property, not by review.
  "\u000b", "li\u000b", "li\u000bspan",
  "\u0085", "li\u0085", "li\u0085span",
  "\u00a0", "li\u00a0", "li\u00a0span",
];

const $ = cheerio.load(FIXTURE);
const run = (sel) => {
  try {
    return {
      ok: true,
      ids: $.root().find(sel).map((i, el) => $(el).attr("id") ?? `<${el.tagName}>`).get(),
    };
  } catch (e) {
    return { ok: false, error: e.name, message: String(e.message).slice(0, 100) };
  }
};

const out = { fixture: FIXTURE, parity: {}, rejected: {} };
for (const sel of PARITY) out.parity[sel] = run(sel);
for (const sel of REJECTED) out.rejected[sel] = run(sel);

const badParity = Object.entries(out.parity).filter(([, r]) => !r.ok);
if (badParity.length) {
  // A selector cheerio cannot run proves nothing about parity, and the Go test refuses to load a
  // baseline containing one. Fail here instead, where the fix is obvious.
  console.error("these PARITY selectors error in cheerio; move them to REJECTED:");
  for (const [sel, r] of badParity) console.error(`  ${sel} => ${r.error}: ${r.message}`);
  process.exit(1);
}

out.cheerio = require("cheerio/package.json").version;
writeFileSync(process.argv[2], JSON.stringify(out, null, 2) + "\n");
console.log(
  "parity:", PARITY.length,
  "| rejected:", REJECTED.length,
  "| of which cheerio answers:", Object.values(out.rejected).filter((r) => r.ok).length,
  "| cheerio", out.cheerio,
);

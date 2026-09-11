# The wire

A page open in HTML Clay can ask a program on your machine to change the file it
is running from, and watch the change arrive. This is the default raw mode.

That is the whole raw mode feature. The page sends a small request, a program in
your terminal edits the `.htmlclay` file, and the edit reaches the page the same
way any other file change does. **No HTML travels on the raw wire in either
direction.** The file is the only thing the two sides share.

It is how you point at a paragraph in a document and say "make this shorter", and
have something local do it. The something can be an AI agent, a shell script, a
formatter, or a person typing. The wire has no opinion.

Structured mode lets programs return data whether or not they also edit the
file. It uses the same request transport, but gives the program a bounded JSON
result and lets the page skip save coordination when the document is not
involved.

## What you need

- HTML Clay 1.6.0 or later, with the file open in a browser
- clayjs 0.6.0 or later, loaded with the `wire` plugin

```html
<script src="https://clayjs.com/v1/clay.js?plugins=sync,wire"></script>
```

`sync` is not required for data only helpers. You want it for edits: without live
sync the page never sees the change the program made, and a request that finished
will say so a few seconds late rather than the moment the text appears.

## Raw mode, the default

### Five minutes

Open a file in HTML Clay, then attach a program to it:

```bash
htmlclay wire serve ~/notes/page.htmlclay -- ./my-agent.sh
```

`my-agent.sh` runs once per request. It gets the request on stdin and the file's
path in the environment:

```bash
#!/bin/bash
echo "working on it"                    # becomes a status the page can show
node rewrite.js "$HTMLCLAY_WIRE_FILE"   # edit the file however you like
```

From the page:

```js
const request = clay.wire.send(
  { instruction: "make the intro shorter", target: "#intro" },
  { text: "make the intro shorter" }
);

clay.wire.on(state => console.log(state.state, state.text));

const outcome = await request.done;   // { state: "done" | "error" | "cancelled", ... }
```

The payload is yours. HTML Clay passes it through untouched, and your program
decides what it means.

## How a raw request travels

```
  page                    HTML Clay                  your program
   |  clay.wire.send()        |                            |
   |------------------------->|  wire/request              |
   |                          |--------------------------->|  runs, prints, edits the file
   |         wire/status      |<---------------------------|
   |<-------------------------|                            |
   |          wire/done       |<---------------------------|  exits 0
   |<-------------------------|                            |
   |                                                       |
   |<== the edited file, through live sync =================|
```

A request moves through these states:

| state | means |
|---|---|
| `sent` | posted, waiting for the program to take it |
| `acked` | the program has it, and every status line rearms the clock |
| `landing` | the program finished writing, the page is waiting to see the bytes |
| `done` | the change is on the page |
| `error` | the program failed, or nothing was attached, or it went silent |
| `cancelled` | you took it back |

**`done` means visible, not merely finished.** The program's exit says the file is
written; the page then waits for live sync to deliver it, up to 4 seconds, before
reporting done. A page with no `sync` plugin still reports done, just later.

Raw mode has 15 seconds to acknowledge, then 120 seconds of silence. **Every
frame rearms the clock**, so a program that prints what it is doing can work for
as long as it likes. A silent one gets two minutes.

## The page API

`clay.wire` exists only when the plugin is loaded, so check for it.

```js
clay.wire.send(payload, opts)   // returns { id, state, done, cancel() }
clay.wire.cancel(id)            // true if it was still open
clay.wire.get(id)               // one snapshot, or undefined
clay.wire.list()                // every request this page knows about
clay.wire.isBusy()              // is anything in flight
clay.wire.on(fn)                // subscribe; returns its own unsubscribe
```

`opts` takes `{ id, type, text, helper, document, onStatus, signal }`, all
optional. `text` is a plain line for a program that wants one without parsing
the payload; `id` lets you supply your own request id, and reusing a live one is
refused as an error rather than thrown. Structured requests use the remaining
options, described below.

`send` returns immediately. `handle.done` resolves with the final snapshot and
**never rejects**: a failed request is an outcome to render, not an exception to
catch.

The DOM event `clay:wire-state` carries the same snapshot, for code that would
rather not hold a subscription.

Requests with `document: "edit"` save the page first. The program is about to
read the file, so it has to read what you are looking at. Autosave is then
suspended until the request ends, so the page cannot write over the program
mid-edit. Both are automatic. This remains the default for an unnamed request.
A named helper defaults to `document: "none"`, which skips the save, autosave
hold, and wait for file changes.

## The raw handler contract

`wire serve <file> -- <cmd>` runs `<cmd>` once per request. Raw mode is the
default. `--protocol=raw` names it explicitly but is not required.

**In:** the whole request envelope as JSON on stdin, plus two environment
variables.

| variable | value |
|---|---|
| `HTMLCLAY_WIRE_FILE` | absolute path of the file to edit |
| `HTMLCLAY_WIRE_ID` | this request's id |

**Out:** every line it prints becomes a `wire/status` frame the page can display.
Exiting `0` becomes `wire/done`, any other code becomes `wire/error`.

**Cancel:** the process is sent `SIGTERM` (Windows has no deliverable SIGTERM, so
it is killed there). A handler that wants to finish its write can trap it.

**On the CLI's own stderr:** the handler's stderr, each line prefixed with the
short request id, and then one line per request saying how it ended (`done`,
`error: …`, `cancelled`). That last line is worth knowing about when something
goes wrong: a request that ends with no terminal frame looks exactly like one
whose frame was posted and lost somewhere downstream, and the two have nothing in
common. The line is printed after the frame goes out, so if the line is there,
the CLI did its part. A request refused before the handler ever ran says so on
the same line (`refused: …`, `already running; ignoring the repeat`).

The program edits the file directly. There is no "return the new HTML" path, on
purpose: the file is the state, and anything that wrote HTML back through the wire
would be a second, competing writer.

## Structured mode

Use structured mode when the answer is JSON data rather than an edit to the
document:

```bash
htmlclay wire serve ~/notes/page.htmlclay --protocol=jsonl -- ./search-helper.sh
```

The relative helper path is correct. Raw mode runs the command from the directory
where you started `htmlclay`. Structured mode resolves the executable there
first, then runs it with the document's directory as its working directory.

Without `--protocol=jsonl`, nothing in this section applies. A JSON looking line
from a raw handler remains plain status text, and its `wire/done` still has no
payload.

### Request and result

The helper receives the request envelope on stdin, as in raw mode. HTML Clay adds
`helperProtocol: 1` before starting it:

```json
{"v":1,"helperProtocol":1,"type":"wire/request","id":"...","file":"/abs/path.htmlclay","helper":"search","document":"none","text":"...","payload":{"query":"needle","target":"notes"}}
```

The helper reserves stdout for UTF-8 JSON Lines records. It emits zero or more
`status` records, followed by exactly one terminal `result` or `error` record:

```json
{"type":"status","text":"Scanning","progress":{"completed":40,"total":100,"unit":"files"}}
{"type":"result","value":{"matches":["notes/a.txt:12:needle"]}}
```

An expected failure is an `error` terminal and an exit status of zero:

```json
{"type":"error","code":"invalid_request","message":"Query is empty","details":{"field":"query"}}
```

HTML Clay holds the terminal record until the child exits zero and stdout reaches
clean EOF. Any record after the terminal, malformed output, a nonzero exit, or a
forced stdout close turns the request into a host error. Status is replaceable
progress and may be dropped, but a record is never truncated into apparent
success. Stderr is for diagnostics and never carries protocol records.

The terminal value reaches the page as `outcome.result`:

```js
const request = clay.wire.send(
  { query: "needle", target: "notes" },
  {
    helper: "search",
    onStatus: ({ text, progress }) => showProgress(text, progress),
    signal: controller.signal
  }
);

const outcome = await request.done;
if (outcome.state === "done") renderMatches(outcome.result.matches);
else showFailure(outcome.error);
```

`document` is either `"edit"` or `"none"`. It defaults to `"edit"` for an
unnamed call and `"none"` for a named helper call. An invalid value is refused.
`onStatus` receives `{ text, progress }`, with `progress` omitted when the helper
did not supply it. `signal` uses the same cancellation lifecycle as
`request.cancel()`.

Structured acknowledgements include `{ "mode": "jsonl", "budgetMs": 300000 }`.
The five minute execution budget does not extend when progress arrives. The page
allows a short delivery and cleanup grace after that budget.

### Record grammar

- Every record is one UTF-8 JSON object. LF, CRLF, and a complete final record at
  EOF without a trailing newline are accepted. Blank lines, pretty printed
  multiline records, and nonobject records are rejected.
- Protocol field names match exactly. Duplicate top level keys are rejected, as
  are duplicate keys inside `progress`. Unknown object fields are ignored. An
  unknown record `type` is an error in version 1.
- `result` requires a `value` member. Explicit `null`, `false`, `0`, `""`, and
  `[]` are valid. A missing member is not `null`.
- `error` requires nonempty string `code` and `message` members. `details` is
  optional JSON.
- `status` requires a string `text`. `progress` is optional. When present, it is
  an object with a nonnegative finite number in `completed`. `total` may be
  absent, `null`, or a nonnegative finite number. `unit`, when present, is a
  string. Neither a percentage nor a known total is required.

### Limits

| boundary | value | what it includes |
|---|---:|---|
| Any stdout line before parsing | 512 KiB | The complete line, excluding its line ending |
| Terminal record | 512 KiB | The complete encoded `result` or `error` record |
| Status record | 32 KiB | The complete encoded record |
| `status.text` and `error.message` | 4 KiB each | Decoded UTF-8 text |
| `error.code` | 128 bytes | Decoded string |
| `progress.unit` | 64 bytes | Decoded string |
| Final encoded wire envelope | 1 MiB | The frame HTML Clay posts |
| Structured execution | 5 minutes | Independent of progress |

The 512 KiB terminal limit includes the record wrapper, so a result value must be
slightly smaller. Results are bounded, not streamed. A helper should cap or
paginate larger answers.

### `wire/describe`

HTML Clay can start a structured helper with a `wire/describe` request before it
sends ordinary work:

```json
{"v":1,"helperProtocol":1,"type":"wire/describe","id":"...","file":"/abs/path.htmlclay","helper":"search"}
```

The helper returns one ordinary terminal result whose value identifies its
contract and operations:

```json
{"type":"result","value":{"helperProtocol":1,"contract":"text-search/1","operations":["search"]}}
```

Description runs use a separate 5 second deadline and an 8 KiB result cap. The
helper should answer from static information. It must not scan folders or start
the operation it is describing.

### A complete shell helper

This helper handles description, validation, progress, no matches, matches, and
tool failure. `jq -cn` produces every record, so shell escaping cannot corrupt
the protocol. The explicit `rg` branch distinguishes no matches, exit 1, from a
real failure.

```sh
#!/bin/sh
request=$(cat) || exit 1
request_type=$(printf '%s' "$request" | jq -er '.type | select(type == "string")') || exit 1

if [ "$request_type" = "wire/describe" ]; then
  jq -cn '{type:"result",value:{helperProtocol:1,contract:"text-search/1",operations:["search"]}}'
  exit $?
fi

if [ "$request_type" != "wire/request" ]; then
  jq -cn '{type:"error",code:"invalid_request",message:"Unsupported request type"}'
  exit 0
fi

query=$(printf '%s' "$request" | jq -er '.payload.query | select(type == "string" and length > 0)') || {
  jq -cn '{type:"error",code:"invalid_request",message:"Query is empty"}'
  exit 0
}
target=$(printf '%s' "$request" | jq -er '.payload.target | select(type == "string")') || {
  jq -cn '{type:"error",code:"invalid_request",message:"Target is missing"}'
  exit 0
}

jq -cn '{type:"status",text:"Scanning"}' || exit 1
if matches=$(rg -n -F -- "$query" "$target"); then
  jq -cn --arg matches "$matches" '{type:"result",value:{matches:($matches | split("\n"))}}'
else
  rc=$?
  if [ "$rc" -eq 1 ]; then
    jq -cn '{type:"result",value:{matches:[]}}'
  else
    exit "$rc"
  fi
fi
```

## The CLI

```
htmlclay wire serve  <file> [--protocol=jsonl] -- <cmd> [args...]
                                                   run <cmd> for every request
htmlclay wire listen <file> [--handler]             print frames as JSON lines
htmlclay wire send   <file> --type <type> ...       send one frame, payload on stdin
htmlclay wire where  <file>                         print the origin serving <file>
```

Flags may sit on either side of the file. `--port <n>` names the origin directly,
which is how you reach a file whose folder HTML Clay does not remember a port for:
read the port off the page's own address bar. `serve` uses raw mode unless you
pass `--protocol=jsonl`.

`listen` is an observer unless you pass `--handler`. The handler slot is exclusive
(one program per file) and it also keeps HTML Clay watching the file while no tab
is open on it, so an edit made while you are away is still versioned and still
appears when you come back.

| exit | means |
|---|---|
| 0 | fine |
| 1 | bad arguments |
| 2 | HTML Clay is not running |
| 3 | the address is held by the recovery page |
| 4 | no site is serving that file, so open it first |
| 5 | another program holds the handler slot |
| 6 | refused |
| 7 | sent, and nothing was attached to take it |

## What keeps this safe

**A local process runs as you, so it needs no secret.** The wire tells a browser
from a program by attestation: a browser sends `Sec-Fetch-Site` or `Origin` on
every request and cannot forge either, and a local process sends neither. That is
the entire classifier. There is no token to leak and no header to copy.

**A page may only reach its own file.** Requests are addressed by the file's
absolute path, and a page can only name the file it was served from. `same-site`
is rejected, not admitted: HTML Clay serves one loopback origin per project tree,
and every one is same-site with every other, so admitting it would let one
project's page drive another project's wire.

**Only a program may be a handler.** A page can send and observe. It cannot claim
the handler slot, so an open tab cannot impersonate your agent.

**Every frame is bounded JSON.** Raw mode carries requests and small status
lines. Structured mode adds a bounded result. Document edits remain in the file,
where you can read them, diff them, and put them in version control.

**Every write is versioned.** An edit made by a program is backed up before it
lands, exactly like one made by the page, and it is in Backups whether or not a
tab was open at the time.

## Things that will trip you up

**Open the file before attaching.** A file registers with HTML Clay on its first
document navigation, and the wire finds files through that registration. Attaching
first exits 4 with "open it first".

**Chrome your script adds must be injected, never authored.** A node marked
`no-save` is left out of the document that gets written, so writing one into the
file by hand means the first save deletes it permanently. Build panels, overlays,
and toolbars at runtime and mark them before connecting them:

```js
node.setAttribute("clay", "no-save no-snapshot no-watch");
for (const t of ["no-save", "no-snapshot", "no-watch"]) node.setAttribute(t, "");
```

Both spellings, because hosts differ on which they read.

**Keys decide what survives.** If someone is editing one part of the page while a
program rewrites another, live sync merges the two by matching elements. Give the
regions a program will touch a stable `data-id` or `id`, or a structural change
has nothing to match and quietly does not appear.

**Print raw status before the work is done.** Frames are dropped for a request
that has already finished, and exiting finishes it. Structured helpers use a
terminal result instead, which HTML Clay holds until clean exit and EOF.

**The first save after a program's edit warns you.** It reports that the file
changed outside this tab and that your version was saved with the previous one in
Backups. Nothing is lost, and the page's own save carries the program's edit
forward. Known wart.

## A worked example

`plans/htmlclay/wire-e2e/` in the Hyper workspace is a runnable harness: a page
with a comment UI, a program on the other end, and five traces that assert the
whole loop against the shipped app. It also contains the small version of the
thing this was built for, a page you can hold a conversation in, where your
comments go to an agent and its replies arrive as edits to the file.

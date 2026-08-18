# The wire

A page open in HTML Clay can ask a program on your machine to change the file it
is running from, and watch the change arrive.

That is the whole feature. The page sends a small request, a program in your
terminal edits the `.htmlclay` file, and the edit reaches the page the same way
any other file change does. **No HTML travels on the wire in either direction.**
The file is the only thing the two sides share.

It is how you point at a paragraph in a document and say "make this shorter", and
have something local do it. The something can be an AI agent, a shell script, a
formatter, or a person typing. The wire has no opinion.

## What you need

- HTML Clay 1.6.0 or later, with the file open in a browser
- clayjs 0.6.0 or later, loaded with the `wire` plugin

```html
<script src="https://clayjs.com/clay.js?plugins=sync,wire"></script>
```

`sync` is not required, but you want it: without live sync the page never sees
the change the program made, and a request that finished will say so a few
seconds late rather than the moment the text appears.

## Five minutes

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

## How a request travels

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

Timeouts, so a stuck program cannot hang the page: 15 seconds to acknowledge, then
120 seconds of silence. **Every frame rearms the clock**, so a program that prints
what it is doing can work for as long as it likes. A silent one gets two minutes.

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

`opts` takes `{ id, type, text }`, all optional. `text` is a plain line for a
program that wants one without parsing the payload; `id` lets you supply your own
request id, and reusing a live one is refused as an error rather than thrown.

`send` returns immediately. `handle.done` resolves with the final snapshot and
**never rejects**: a failed request is an outcome to render, not an exception to
catch.

The DOM event `clay:wire-state` carries the same snapshot, for code that would
rather not hold a subscription.

**Sending saves the page first.** The program is about to read the file, so it has
to read what you are looking at. Autosave is then suspended until the request
ends, so the page cannot write over the program mid-edit. Both are automatic.

## The handler contract

`wire serve <file> -- <cmd>` runs `<cmd>` once per request.

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

The program edits the file directly. There is no "return the new HTML" path, on
purpose: the file is the state, and anything that wrote HTML back through the wire
would be a second, competing writer.

## The CLI

```
htmlclay wire serve  <file> -- <cmd> [args...]   run <cmd> for every request
htmlclay wire listen <file> [--handler]          print frames as JSON lines
htmlclay wire send   <file> --type <type> ...    send one frame, payload on stdin
htmlclay wire where  <file>                      print the origin serving <file>
```

Flags may sit on either side of the file. `--port <n>` names the origin directly,
which is how you reach a file whose folder HTML Clay does not remember a port for:
read the port off the page's own address bar.

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

**Nothing but text crosses.** Requests and status lines are small JSON frames.
The content is in the file, where you can read it, diff it, and put it in version
control.

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

# Security

HTML Clay has two different security boundaries. Keep them separate:

1. **The HTML Clay server limits what a page can read and save through HTML Clay.** A page normally changes only itself. A trusted folder is the exception: every HTML Clay file in it can read named files in the folder and can acquire the save capability for every other HTML Clay file in it.
2. **A registered helper program runs as you.** It has your access to files, processes, environment variables, and the network. HTML Clay decides whether to start the program. It does not sandbox the program or constrain what the program does after it starts.

A document is not the program being approved. A document asks for a helper by a short name. You select the program when it is registered, and the stored document decision points to that program's local ID. HTML Clay does not let the document supply an executable path or command line, but the document supplies the request on the program's standard input. The program must treat every request as hostile input.

## Registered helper programs

A document declares helpers in `<meta name="htmlclay-helper">` elements near the start of its `<head>`. HTML Clay reads at most the first 512 KiB and accepts at most eight valid, distinct names. Later declarations are silently ignored.

The decision happens when HTML Clay receives a direct file open, before that page's script runs. For every name with no stored decision, HTML Clay raises one native dialog. The dispatcher never raises a permission dialog in response to a request. A denied helper call fails immediately.

A linked `.htmlclay` file can auto-register when it is opened inside a trusted folder. The current auto-registration path does not inspect helper declarations, ask for helper permission, or attach a dispatcher. Such a file has no helper access until it is opened directly. This differs from the general plan that authorization happens whenever a document opens.

The dialog offers two affirmative choices. Neither is a one-time grant:

- **"Allow for This Document"** allows the selected program for that one document, across reloads and restarts.
- **"Allow for Any Document"** allows the selected program for every document that declares the same helper name. It is not limited to one folder.

The tray's row for a program turns any-document access back off and forgets the stored document decisions.

The prompt lists helper names but not the registered program paths. If more than one registered program has the same name, the current code selects the earliest registration without asking which program to use. The saved decision still identifies one concrete program, but the prompt does not tell you which one. This is also release blocking because a user cannot knowingly approve the program that will run.

A refusal is stored and the document does not ask again, including after restart. It is supposed to remain permanent until changed in the tray. In the current build, the tray's "Forget document permissions" action can clear allowed decisions but cannot clear a refusal, because a refusal stores no program ID. A refusal therefore cannot currently be changed in the tray. This is a release blocking limitation.

### What approval gives the program

For each request, HTML Clay starts the registered path with the document's folder as its working directory. It passes the full request as JSON on standard input, inherits the app's environment, and supplies the document path and request ID in environment variables. On macOS and Linux it also supplies a login shell `PATH` so ordinary script interpreters can be found.

The five minute execution deadline, the structured output parser, and the result size limits protect HTML Clay from an unbounded request or malformed response. They do not restrict the program's operating system access. A helper can, among other things:

- Read, create, change, or delete any file your account can access, including files outside the document folder and files HTML Clay itself refuses to serve.
- List directories, start other programs, and leave descendant processes running.
- Use the network and send away anything it can read.
- Read or change HTML Clay's `config.json`, including its program registry and permission decisions. The file is mode 0600, but the helper runs as the same account that owns it.

HTML Clay permits up to eight concurrent helper requests per document. This is not an app wide limit. Several documents can each run eight children, so the limit is not resource isolation.

Register only programs you trust with your account. The security boundary inside a helper is what that program does with hostile standard input. Validate the operation, types, paths, sizes, and every other value before using them. HTML Clay validates the helper's output protocol. It cannot validate the meaning of the input for the program.

HTML Clay stores the selected program path, not a hash or a pinned file identity. Replacing the program at that path, or changing the target behind a selected symlink, changes what runs without another prompt. Whoever can update that program controls the code future requests execute.

### Who can invoke an approved program

The dispatcher accepts only names declared by that document and allowed in its stored decisions. Helper wire sends and subscriptions also require that document's save token. Another HTML Clay origin and a local process without the token are refused.

Files in one trusted folder share an origin. A hostile file in that folder can open an approved sibling in a visible top level tab, read the sibling's save token through their shared origin, and use that token to reach the sibling's approved helper. Document specific approval therefore does not isolate a helper from hostile files in the same trusted folder.

When a document has a helper dispatcher, both live sync relay addresses refuse page supplied updates to that target, on both relay lanes. This prevents a sibling from silently pushing a script into an already open, helper enabled tab. It does not stop the visible navigation and token path above, and it does not limit the helper after execution starts.

### Removing or revoking a program

Removing a program or its document permissions stops future dispatch. HTML Clay advances the document's helper generation, rejects late output, and cancels work it is tracking.

That cancellation is not containment. The runner signals only the direct child process and waits for it. Descendants can survive, including descendants that keep output pipes open. Process group cleanup, if added, would be tidying rather than a security boundary because a program running as the user can escape the group or arrange work elsewhere.

Removing or revoking a program does not undo writes, recover data already sent away, or stop work that escaped the tracked process. It also cannot protect the registry from that program, because the registry is owned by the same account.

Removing a program leaves every document decision that pointed at it in place. Each one reads as undecided while the program is gone, so the next open asks again, and restoring the same program ID makes those decisions grant once more. Registering a new program under the same display name does not inherit them: registration mints a new ID, and the stored decisions name the old one.

## What HTML Clay itself protects

These protections apply to requests handled by HTML Clay. They are not a sandbox around registered programs.

- **Only files you choose can be saved through HTML Clay.** That choice takes two forms: a file you open yourself, and any HTML Clay file inside a folder you have trusted. Everything else, including a file reached by a link, an iframe, or a typed URL, is served read only and never gets a save token.
- **A page can only read through HTML Clay inside the folder it was opened from.** A request outside that folder pauses and asks you, in a dialog HTML Clay draws itself, to widen reads to one named folder.
- **Each project gets its own origin.** A trusted folder runs on its own local port. Each folder containing an opened loose file also gets its own port. The browser therefore treats unrelated projects as different sites.
- **Save tokens ride only on real navigations.** A silent background `fetch()` of another file receives no save token. Getting a sibling's token requires a visible top level document navigation.
- **The read only banner cannot select another file.** Its single use code resolves on the server to the exact file that was served. The code expires after ten minutes and dies on first use.
- **Always refused, trusted or not:** anything outside your home folder, dotfiles and dot-directories such as `.env`, `.git`, and `.ssh`, HTML Clay's own settings and version history, and directory listings.
- **Other websites cannot drive the server.** It listens only on loopback, validates the `Host` header, rejects cross site browser requests, and requires same origin browser attestation on routes that save, restore, upload, relay live updates, or change folder permissions.
- **Permission prompts are native.** Page content cannot draw, restyle, or click them.
- **Reads are judged by the file actually opened.** HTML Clay checks the real path reported for the open handle, so swapping a symlink during a request cannot redirect the read into its own settings or version history.
- **A refused read reveals no target path.** Out of scope denials use one fixed response and are decided before checking whether the requested file exists.
- **JSON data routes use the same read checks.** The `?data=` and `/_/api/` routes cannot reach a file an ordinary request could not, never supply a save token, and strip any token that was written to disk.

## Trusted folders

A trusted folder is HTML Clay's durable write grant. Every HTML Clay file in it, now or later, can become editable without another prompt and can follow links to other HTML Clay files in the folder.

Plainly: **one hostile HTML file inside a trusted folder can rewrite every other HTML Clay file in that folder.** It can open each sibling in a visible tab, read that page's save token through the shared origin, and overwrite it. A silent background fetch still receives no token, so this path is visible, but it is possible.

Version history records saves made through HTML Clay and normally keeps at least the last 20 versions plus everything from the last 60 days. It helps recover from page based overwrites. It is not protection from a helper, which can change or delete the version store with the user's file permissions.

Untrusting a folder closes that origin, revokes registrations created only through the trust, closes their live streams, and rechecks any later save. Files you opened yourself remain open on another origin because opening a file was a separate decision. Stored helper decisions for those files remain separate from folder trust.

Each trusted folder is normally pinned to the folder identity reported by the operating system. If the folder is deleted, replaced, or changed into a link to another folder, the entry stops granting and appears as missing or replaced. If the filesystem cannot supply a stable identity, HTML Clay falls back to treating the stored path as the identity.

## Known limitations

1. **A trusted folder trusts everything in it.** A hostile file can read that tree, send data away, overwrite sibling HTML Clay files, and reach a sibling's approved helper after acquiring its token through a visible navigation.
2. **A page can steer a read permission prompt.** It chooses the requested path. Read the full path before allowing it. Page steered trust refuses your main personal folders and everything inside them. You can still choose a folder deliberately from the tray.
3. **The Linux and Windows prompts still need native verification.** They are covered by tests and fail closed when a dialog process fails, but they have not been exercised on those operating systems for this release.
4. **Mixed capitalization across mounted filesystems is not fully handled.** HTML Clay detects the rule used by the home filesystem. A mounted filesystem with the opposite rule can still be compared incorrectly.
5. **Unicode equivalent folder names can briefly produce duplicate trusted entries.** Load normalization merges entries that the operating system reports as the same directory, but two equivalent spellings added during one run can coexist until restart. Remove every duplicate row if you see one.
6. **Some Linux desktops cannot draw every permission choice.** With kdialog and no zenity, the durable third folder permission choice is unavailable and degrades to the narrower choice. Program management still uses a radiolist.
7. **Hard links are not a boundary against a local attacker.** Someone who can already create hard links inside your home folder can link files into an allowed tree. That attacker already has local file access.
8. **Inside a folder you allowed or trusted, a page can tell which files exist.** That follows from read access.
9. **A hostile page can cause repeated prompts for invented folders.** Allowing an invented folder grants nothing. Denying suppresses that branch for the session, but the page can invent other names.
10. **A fresh helper subscription may not recover a missed outcome.** Terminal result retention is bounded. Do not automatically repeat a helper request merely because its result was lost, since the program may already have completed side effects.

## Reporting a problem

Email david@storylog.com with what you found and how to reproduce it. Please do not open a public issue for a security problem until it is fixed.

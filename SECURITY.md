# Security

HTML Clay serves your files to your own browser through a small server that listens only on your own machine. Two rules carry the whole design:

1. **Only files you explicitly open can be saved.** A file reached by a link, an iframe, or a typed URL is served read-only and never gets a save token.
2. **A page can only read inside the folder you opened it from.** A request for anything outside that folder pauses and asks you, in a dialog HTML Clay draws itself, to widen reads to one named folder, read-only.

Everything else below is detail on those two rules, plus an honest list of what they do not cover.

## What protects what

- **Each tree gets its own origin.** Every folder you open runs on its own local port, so your browser treats two open projects as two different sites and will not let one read the other.
- **Grants are reads only.** Allowing a folder widens reading, never writing. "Allow Once" is forgotten when you quit, and you can take it back sooner from the tray, under "Temporary Access Granted". "Trust this folder" also records the folder under Trusted Folders, where you can remove it whenever you like. Removing it there also ends the read access it granted.
- **Trusted Folders are anchored to where you opened from.** Marking a folder as trusted lets files opened from inside it serve and self-save with no prompts, and read that folder's tree silently. A file opened somewhere else gets nothing: a page in `~/Downloads` cannot read a trusted `~/sites`.
- **Always refused, granted or not:** anything outside your home folder, dotfiles and dot-directories such as `.env`, `.git`, and `.ssh`, HTML Clay's own settings and version history, and directory listings.
- **Other websites cannot reach it.** The server checks the `Host` header, rejects cross-site requests, and listens only on the loopback address, so a page on the open internet cannot drive it.
- **Permission prompts are never drawn by a page.** Only HTML Clay itself can raise the Allow or Deny dialog, so a page cannot fake, restyle, or auto-click one.
- **Reads are judged by the file actually opened**, using the real path the operating system reports for the open handle, not the name in the request. Swapping a symlink halfway through a request cannot redirect a read into HTML Clay's own state.
- **A refused read tells a page nothing.** Denials are a single fixed response that names no path, and the answer does not depend on whether the file is there, so a page cannot use refusals to work out which files you have.

## Known limitations

These are real and current. They are listed here rather than left to be discovered.

1. **A trusted folder trusts everything in it.** A hostile HTML file placed inside a trusted folder can read that whole tree and send it somewhere when you open it. It still cannot overwrite your other files. Only trust folders where you control the HTML and JavaScript.
2. **The Linux and Windows prompts are beta.** They are implemented and reviewed but have not yet been exercised on real Linux and Windows machines. They fail closed: if a dialog cannot run, access is denied. The realistic failure is a permission that will not grant, not one that silently succeeds. Please report anything that looks wrong.
3. **A page can offer you one-click trust of a folder it chose.** The dialog's "Trust this folder" button remembers the folder, so anything you later open from inside it stops asking. The folder named in that dialog is picked by the page, through which files it asks for, so read the path before you click it. Your main personal folders, including Desktop, Documents, and Downloads, are refused on this route, since those are where files you did not write tend to land. You can still trust one deliberately from the HTML Clay menu. On Windows, and on Linux without zenity, this button falls back to "Allow Once", which grants less rather than more.
4. **Case-sensitive disks are not fully handled.** HTML Clay assumes macOS and Windows ignore capitalization in filenames. On a case-sensitive volume inside your home folder, two folders whose names differ only in case can be treated as one, so a grant may cover more than the dialog named, and taking back one spelling can leave the other in place. A fix is planned.
5. **Hard links.** Someone who can already create hard links inside your home folder could link HTML Clay's own files into a folder you then grant. That takes an attacker who is already on your machine and can read those files directly, so it adds no exposure they did not already have.
6. **Inside a folder you granted, a page can tell which files exist.** That comes with read access and is not separately preventable.
7. **A page can make HTML Clay ask about a folder that does not exist.** Because the question is decided from the path a page requests rather than from what is on disk, a page can name any folder, including invented ones, and get a prompt. Denying one stops that whole branch from asking again, but a page can keep inventing new names, so a hostile page can be annoying. Allowing an invented folder grants nothing. This is the cost of the prompt looking the same whether or not the file exists.

## Reporting a problem

Email david@storylog.com with what you found and how to reproduce it. Please do not open a public issue for a security problem until it is fixed.

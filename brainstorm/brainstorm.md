# Malleable HTML Files — Brainstorm

## The Vision

A single HTML file is the perfect container for a malleable application. It opens in a native browser that already functions as both a viewer and an editor — you can click buttons, move things around, edit text, audio, video, and run a full multimedia editing suite, often in under 5 megabytes. Most of that power is already built into the browser itself.

## The Problem

As a developer, I can build out an HTML file in a code editor — script block, body, head, styles — and understand it at a glance. I can send it to a friend and they can use it on their computer to process files or whatever they need. But if they want to *edit* the file and send it back, the options fall apart:

- **localStorage**: Not transferable. If they package up the HTML and send it back, their localStorage doesn't come with it.
- **Edit the code directly**: Regular users aren't going to do that.
- **Download a new copy**: Solutions like TiddlyWiki do this, but it's not easy for a normal user. They click a download button, it lands in some directory, and now they have to figure out which file is the original and which is the updated one. It should just be one file.

## The Wish List

The ideal, top-of-the-line solution:

1. I send someone an HTML file.
2. They open it in **any browser** (Firefox, Chrome, Safari) under the **file protocol**.
3. They can load modules, CSS, whatever they need.
4. They change the page using the page's own interface.
5. **It saves back to disk** — same file, overwritten in place.
6. They can also upload it to a platform that hosts these malleable HTML files, edit it there, and other people can fork it, copy it, or subscribe to it.

The only thing actually required for this is a browser permission change: allow an HTML file opened via `file://` to **overwrite itself**. That's it. Not system access, not access to other files — just the ability to write back to itself.

## Why Doesn't This Exist?

There's a strange divide between the text representation and the visual representation of an HTML file:

- In a **text editor**, you have total freedom — paste whatever you want, save directly.
- In a **browser**, you have almost no control over the file's contents on disk.

Yet other file formats get this privilege:

- Photoshop files can be double-clicked, edited, and saved.
- Illustrator, Google Docs, Microsoft Office — same thing.
- All of them have vulnerabilities. That's how a lot of modern-day exploits spread.
- Many of these are networked too — Figma, Google Docs, Outlook files all trigger remote network requests.

So why is HTML locked down differently? Possibly because:

- **HTML is an open standard.** Giving people the power to create full applications without relying on closed-source systems may not align with the incentives of platform owners.
- **HTML is 10-100x more powerful than a Word document.** The attack surface is enormous.
- **Open standards bodies can't move as fast** as Microsoft or Adobe to ship security patches when vulnerabilities are found.
- **Closed-source companies are strongly incentivized** (shareholders, ecosystem reputation) to patch quickly.

## Proposed Solutions

### Option 1: Build a Custom Browser

Build a browser that's a perfect copy of Chrome with one addition: HTML files get permission to overwrite themselves. Way too much work — but it would be amazing.

### Option 2: A Special File Extension (Preferred Direction)

Create a custom file type — something like `.malleable` — that:

- Is **literally just an HTML file**. The contents are regular HTML, nothing special.
- The extension exists solely to **associate with a custom application** that opens it with relaxed permissions.
- Double-clicking a `.malleable` file opens it in a browser-like environment (possibly a localhost server behind the scenes) where the file can overwrite itself.
- Opening it in a text editor works too — editors just need to associate `.malleable` with HTML syntax, and it's regular HTML from there.

The user experience:

1. Install one application (maybe called "Malleable" or "Malleable Hypermedia").
2. Receive a `.malleable` file.
3. Double-click it.
4. Edit it visually in a browser interface, or open it in a text editor.
5. Changes save back to the same file automatically.

Behind the scenes, the app might spin up a localhost server to handle the `file://` protocol limitations (module loading, cross-origin requests, etc.), but the user never has to think about that.

## Open Questions

- How does [Lobe Code](https://lobe.ai/) handle this? It apparently works with the file protocol somehow.
- Can this be done safely and ethically, given the security concerns around open HTML files?
- What's the minimal viable version of the custom app — an Electron shell? A native wrapper? A CLI that opens the default browser?
- Is there a way to avoid the custom extension entirely and just make it work with `.html`?

---

*— David*

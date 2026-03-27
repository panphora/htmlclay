
Lopecode operates over the file:// protocol by employing a microkernel architecture that treats the browser's memory as the primary storage and the HTML file as a "bootable" image. It overcomes local file limitations through live runtime modification, IndexedDB storage for persistence, and an Exporter module that serializes the entire state into a downloadable file. For more details, visit [tomlarkworthy.github.io](https://tomlarkworthy.github.io/lopecode/).
[1, 2, 3] 

[1] [https://github.com](https://github.com/tomlarkworthy/lopecode)
[2] [https://tomlarkworthy.github.io](https://tomlarkworthy.github.io/lopecode/)
[3] [https://github.com](https://github.com/tomlarkworthy/lopecode#:~:text=Lopecode%20is%20a%20web%2Dbased%20modable%20programming%20environment,no%20UI%20and%20defines%20programming%20mechanisms%20for.)


The trick is that Lopecode is **not** trying to make `file://` behave like a normal hosted web app. It changes the architecture so that almost everything happens **inside one self-contained HTML file**, using browser features that still work locally: JavaScript execution, the DOM, reactive in-memory state, browser-local storage, user-selected files, and export/download. Tom Larkworthy’s own README says the kernel is only three things—**reactive dataflow, hot code swapping, and modules**—and that the Exporter can serialize the entire runtime into **a single HTML file** that works offline from the local filesystem with “no dependencies other than a web browser.” ([GitHub][1])

The reason this feels surprising is that `file://` really is limited in some important ways. MDN says `file://` is “generally” treated as potentially trustworthy, but local files are also often treated as having **opaque origins**, which breaks many normal cross-file and cross-origin patterns. MDN also notes that `fetch`/XHR/CORS requests only work over HTTP/HTTPS, and that loading local resources from local files now often triggers CORS problems because browsers no longer assume neighboring files share an origin. ([MDN Web Docs][2])

So the high-confidence model is:

* **Lopecode avoids needing a server in the first place.** It packages the runtime, modules, editor, and dependencies into one HTML artifact instead of expecting separate JS/CSS/assets to be fetched over HTTP. Tom’s exporter posts explicitly say it serializes notebooks into a single file that works from `file://`, stays unminified, works offline, and is recursively self-sustaining. ([The Observable Forum][3])

* **Live editing / hot reloading comes from the Observable runtime, not from the file protocol.** Lopecode’s microkernel exposes reactive dataflow, hot code swapping, and modules; the Observable runtime itself supports `module.redefine` and `variable.define`, which is how a cell can be redefined and all dependents recomputed without reloading the page. In other words, the “editability” is happening inside the browser’s already-running JS runtime. ([GitHub][1])

* **Self-editing comes from treating the runtime as the source of truth.** Tom says there is “no external source code”; instead the runtime is **decompiled on demand**. His toolchain notebook says the compiler turns notebook source cells into reactive runtime variables, and the decompiler goes back from live runtime definitions to source. That lets Lopecode reconstruct editable code from the live system rather than from external `.js` files. ([GitHub][1])

* **The single-file export works by embedding code and assets into the HTML itself.** Tom says the exporter bundles dependencies and even file attachments into the single file. One exporter note says file attachments are encoded as **base64 strings plus metadata** in the document. So instead of relying on `file://` to load lots of neighboring files, Lopecode packs those resources into the page. ([The Observable Forum][4])

* **Module loading is handled inside the exported page rather than by relying on normal static imports from disk.** Tom wrote that Exporter 2 moved to `es-module-shims` because it enables dynamically loaded modules and “advanced local-first workflows” for CSS, WASM, and TypeScript while still staying in a single file. In a GitHub discussion he also said imported-module conflicts were being fixed via **import path rewriting** so dependencies line up correctly in exported notebooks. ([The Observable Forum][3])

* **Persistence is split into two different kinds, and that’s the key thing to understand.**

  1. **Program persistence**: save the current system as a new exported HTML snapshot.
  2. **App-data persistence**: use browser-local storage layers for working state.
     Lopecode’s “Notes” example is described as an offline notetaking app using **Dexie.js**; the Observable snippet says notes are stored in the browser using “local-first Dexie,” and you can export the whole application including notes. Jumpgate’s snippet says its git checkout state is persisted to **IndexedDB** using **lightning-fs** and isomorphic-git. The lightning-fs README says it chose IndexedDB specifically because it needed persistence and performance, and that `localStorage` was not suitable. ([GitHub][5])

* **Attachments and large local files are handled as user-provided browser data, not arbitrary filesystem access.** Tom’s “Writable FileAttachments” notebook says you can attach files to notebooks **without uploading them anywhere**, including large files, and get programmatic access to them. Separately, MDN’s File API says web apps can read files **when the user makes them available**, such as with an `<input type="file">` or drag-and-drop. ([Observable][6])

* **Some features still fall back to the network when available, but they do not have to.** Tom explains that `importNotebook` first looks for a **local version** and then falls back to the Observable API. That tells you the architecture is hybrid: local-first when the module is already present in the exported/runtime image, network-assisted when you’re importing something new and online. ([GitHub][7])

What Lopecode **cannot** magically do on plain `file://` is just as important:

* It cannot safely assume normal cross-file `fetch()` / XHR / font / stylesheet loading will work the way it does on `http://localhost`; MDN says those patterns often fail on local files because browsers now treat local files as opaque-origin by default. ([MDN Web Docs][8])
* It should not rely on `localStorage` semantics on `file://`; MDN says behavior there is **undefined** and varies by browser, even if it seems to work. ([MDN Web Docs][9])
* It cannot count on directly overwriting arbitrary files on disk from a plain local HTML page. MDN says the File System Access API is available only in **secure contexts**, and the general File API only gives access to files the user explicitly provides. So the safe assumption is: Lopecode’s plain `file://` path is based on **exporting/downloading new HTML snapshots** plus browser-local databases, not silently mutating the source file on disk. ([MDN Web Docs][10])

So the clean mental model is:

**Lopecode works on `file://` because it behaves more like a self-contained runtime image than a typical web app.**
It keeps code, tools, and assets inside one HTML file; edits live state inside the browser via the Observable runtime; reconstructs source by decompiling runtime state; persists app data in browser-local stores like IndexedDB/Dexie/lightning-fs when needed; and uses export to turn the current image back into a durable HTML artifact. It is mostly **avoiding** the weak parts of `file://`, not overcoming them. ([GitHub][1])

One caveat: I could map the major mechanisms with high confidence from Tom’s README, notebooks, forum posts, and the Observable/runtime docs, but some notebook internals are JS-heavy and don’t expose every implementation detail in static text. The big pieces are clear; the exact backing store for every auxiliary module is not fully visible from the public text alone. ([GitHub][5])

I can do a second pass that turns this into a very concrete matrix like “feature → exact browser primitive → why it works on `file://` → where it breaks.”

[1]: https://github.com/tomlarkworthy/lopecode/blob/main/README.md "lopecode/README.md at main · tomlarkworthy/lopecode · GitHub"
[2]: https://developer.mozilla.org/en-US/docs/Web/Security/Defenses/Secure_Contexts "Secure contexts - Security | MDN"
[3]: https://talk.observablehq.com/t/single-file-notebook-export-format-implemented-in-userspace/10013 "Single File Notebook export format implemented in userspace - Show and Tell - The Observable Forum"
[4]: https://talk.observablehq.com/t/notebooks-as-offline-first-single-webfiles/10145?utm_source=chatgpt.com "Notebooks as offline-first single webfiles?! - Show and Tell"
[5]: https://github.com/tomlarkworthy/lopecode "GitHub - tomlarkworthy/lopecode: moldable substrate for web and offline · GitHub"
[6]: https://observablehq.com/%40tomlarkworthy/fileattachments?utm_source=chatgpt.com "Writable FileAttachments / Tom Larkworthy"
[7]: https://github.com/tomlarkworthy/lopecode/discussions/50 "How to create a new, blank module from an offline notebook · tomlarkworthy lopecode · Discussion #50 · GitHub"
[8]: https://developer.mozilla.org/en-US/docs/Web/HTTP/Guides/CORS/Errors/CORSRequestNotHttp "Reason: CORS request not HTTP - HTTP | MDN"
[9]: https://developer.mozilla.org/en-US/docs/Web/API/Window/localStorage "Window: localStorage property - Web APIs | MDN"
[10]: https://mdn2.netlify.app/en-US/docs/Web/API/File_System_Access_API "File System Access API - Web APIs | MDN"

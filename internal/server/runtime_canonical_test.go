package server

import (
	"fmt"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/panphora/htmlclay/internal/clayruntime"
	"github.com/panphora/htmlclay/internal/session"
	"github.com/panphora/htmlclay/internal/specwire"
)

// localRuntimeDocument is a document holding what a browser that saved what it was served
// would write: this host's runtime URL and the descriptor that turns it back.
func localRuntimeDocument(content string) string {
	return `<!DOCTYPE html>` + "\n" +
		`<html lang="en">` + "\n" +
		`<head>` + "\n" +
		`<script src="/_/clayjs/` + clayruntime.Selected + `/clay.js"></script>` + "\n" +
		`<script type="application/json" id="clay-runtime-map" clay="no-save no-watch no-snapshot">{"schema":1,"version":"` +
		clayruntime.Selected + `","entries":[{"original":"https://clayjs.com/v1/clay.js","local":"/_/clayjs/` +
		clayruntime.Selected + `/clay.js"}]}</script>` + "\n" +
		`</head>` + "\n" +
		`<body><p>` + content + `</p></body>` + "\n" +
		`</html>` + "\n"
}

// A browser saves what it holds, and what it holds is the projected page. Nothing a tab
// round-trips to disk may name this host, and the save still answers with the stamp of the
// bytes it stored.
func TestRuntimeCanonicalSaveWritesHostIndependentBytes(t *testing.T) {
	srv, _, _ := setupHandlerTest(t)
	page := writeHome(t, srv, "kanban.htmlclay", kanbanDocument())
	f, err := srv.sessions.Register(page, session.ViaOsOpen)
	if err != nil {
		t.Fatal(err)
	}

	served := serve(t, srv, "/kanban.htmlclay", nil)
	if served.Code != 200 {
		t.Fatalf("serve: %d %s", served.Code, served.Body.String())
	}
	if !strings.Contains(served.Body.String(), "/_/clayjs/") {
		t.Fatalf("the served document was not projected:\n%s", served.Body.String())
	}
	etag := decodedDocumentETag(t, served.Body.Bytes())
	if etag == "" {
		t.Fatal("the served document carries no documentetag to save against")
	}

	edited := strings.Replace(served.Body.String(), "<p>board</p>", "<p>board edited</p>", 1)
	if edited == served.Body.String() {
		t.Fatal("the served document has no board text node to edit")
	}
	w := saveThroughMux(t, srv, f, edited, etag)
	if w.Code != 200 {
		t.Fatalf("save: %d %s", w.Code, w.Body.String())
	}

	disk, err := os.ReadFile(page)
	if err != nil {
		t.Fatal(err)
	}
	for _, local := range []string{"/_/clayjs/", "clay-runtime-map"} {
		if strings.Contains(string(disk), local) {
			t.Errorf("the saved document kept %q on disk:\n%s", local, disk)
		}
	}
	for _, want := range []string{
		`src="https://clayjs.com/v1/clay.js?plugins=sortable,people,upload,sync,visitor-drafts,unsent,menu&exclude=richclay"`,
		`src="https://clayjs.com/v1/sap.js"`,
		`href="https://clayjs.com/v1/src/page/themes/light-1.css"`,
		"board edited",
	} {
		if !strings.Contains(string(disk), want) {
			t.Errorf("the saved document lost %q:\n%s", want, disk)
		}
	}
	if got, want := decodeJSON(t, w)["etag"], specwire.Etag(disk); got != want {
		t.Errorf("the save answered with etag %v, want %q (specwire.Etag of the disk bytes)", got, want)
	}

	again := serve(t, srv, "/kanban.htmlclay", nil)
	if again.Code != 200 {
		t.Fatalf("serve after the save: %d %s", again.Code, again.Body.String())
	}
	if !strings.Contains(again.Body.String(), "/_/clayjs/"+clayruntime.Selected+"/clay.js") ||
		!strings.Contains(again.Body.String(), `id="clay-runtime-map"`) {
		t.Errorf("the saved document was not projected when served again:\n%s", again.Body.String())
	}
}

// A client with no descriptor and a client carrying a hostile one both end up naming the
// public release, never this host and never somebody else's.
func TestRuntimeCanonicalSaveTurnsLocalURLsPublic(t *testing.T) {
	t.Run("no descriptor", func(t *testing.T) {
		srv, f, _ := setupHandlerTest(t)
		body := `<!DOCTYPE html>` + "\n" + `<html lang="en">` + "\n" +
			`<head><script src="/_/clayjs/` + clayruntime.Selected + `/clay.js"></script></head>` + "\n" +
			`<body><p>board</p></body>` + "\n" + `</html>`

		if w := saveThroughMux(t, srv, f, body, ""); w.Code != 200 {
			t.Fatalf("save: %d %s", w.Code, w.Body.String())
		}
		assertDiskDocument(t, f.AbsPath, `src="https://clayjs.com/`+clayruntime.Selected+`/clay.js"`)
	})

	t.Run("hostile descriptor", func(t *testing.T) {
		srv, f, _ := setupHandlerTest(t)
		body := `<!DOCTYPE html>` + "\n" + `<html lang="en">` + "\n" +
			`<head><script src="/_/clayjs/` + clayruntime.Selected + `/clay.js"></script>` +
			`<script type="application/json" id="clay-runtime-map">{"schema":1,"version":"` +
			clayruntime.Selected + `","entries":[{"original":"https://evil.example/x.js","local":"/_/clayjs/` +
			clayruntime.Selected + `/clay.js"}]}</script></head>` + "\n" +
			`<body><p>board</p></body>` + "\n" + `</html>`

		if w := saveThroughMux(t, srv, f, body, ""); w.Code != 200 {
			t.Fatalf("save: %d %s", w.Code, w.Body.String())
		}
		assertDiskDocument(t, f.AbsPath, `src="https://clayjs.com/`+clayruntime.Selected+`/clay.js"`)
		disk, err := os.ReadFile(f.AbsPath)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(disk), "evil.example") {
			t.Errorf("a descriptor chose where a runtime URL points:\n%s", disk)
		}
	})
}

// assertDiskDocument reads a document back and requires want in it and no projection in it.
func assertDiskDocument(t *testing.T, path, want string) {
	t.Helper()
	disk, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(disk), want) {
		t.Errorf("the document on disk is missing %q:\n%s", want, disk)
	}
	for _, local := range []string{"/_/clayjs/", "clay-runtime-map"} {
		if strings.Contains(string(disk), local) {
			t.Errorf("the document on disk kept %q:\n%s", local, disk)
		}
	}
}

// A document with no runtime URL is written exactly as sent, and the refusals in front of the
// write keep their order with or without one.
func TestRuntimeCanonicalSaveLeavesOrdinarySaveAlone(t *testing.T) {
	srv, f, _ := setupHandlerTest(t)
	plain := `<!DOCTYPE html>` + "\n" + `<html lang="en">` + "\n" +
		`<head><title>Two</title></head>` + "\n" + `<body><p>Two</p></body>` + "\n" + `</html>`

	if w := saveThroughMux(t, srv, f, plain, ""); w.Code != 200 {
		t.Fatalf("save: %d %s", w.Code, w.Body.String())
	}
	disk, err := os.ReadFile(f.AbsPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(disk) != plain {
		t.Errorf("an ordinary save was rewritten:\ngot  %q\nwant %q", disk, plain)
	}

	for name, body := range map[string]string{
		"no runtime URL": plain,
		"local runtime URL": `<!DOCTYPE html>` + "\n" + `<html lang="en">` + "\n" +
			`<head><script src="/_/clayjs/` + clayruntime.Selected + `/clay.js"></script></head>` + "\n" +
			`<body><p>stale</p></body>` + "\n" + `</html>`,
	} {
		t.Run("stale "+name, func(t *testing.T) {
			before, err := os.ReadFile(f.AbsPath)
			if err != nil {
				t.Fatal(err)
			}
			w := saveThroughMux(t, srv, f, body, `"not-the-current-stamp"`)
			if w.Code != 412 {
				t.Fatalf("stale save: %d, want 412 (%s)", w.Code, w.Body.String())
			}
			after, err := os.ReadFile(f.AbsPath)
			if err != nil {
				t.Fatal(err)
			}
			if string(after) != string(before) {
				t.Errorf("a refused save wrote to disk:\ngot  %q\nwant %q", after, before)
			}
		})
	}
}

// A data write starts from the stored bytes, so it can only ever write back what disk already
// holds. The write engine refuses an edit inside a script or a link, which are the only places a
// runtime URL lives, so what this can show is that a canonical document stays canonical.
func TestRuntimeCanonicalDataWriteKeepsDiskCanonical(t *testing.T) {
	srv, _, _ := setupHandlerTest(t)
	body := `<!DOCTYPE html>` + "\n" + `<html lang="en">` + "\n" +
		`<head>` + "\n" +
		`<script src="https://clayjs.com/` + clayruntime.Selected + `/clay.js"></script>` + "\n" +
		`<script type="application/json" data-rules-name="api" data-rules-version="1">{title:"script@src",text:"p"}</script>` + "\n" +
		`</head>` + "\n" +
		`<body><p>board</p></body>` + "\n" + `</html>` + "\n"
	page := writeHome(t, srv, "test.htmlclay", body)

	// The runtime URL lives in a script, which the content-only policy refuses to edit.
	if w := postDataWrite(t, srv, "/_/api/test.htmlclay",
		`{"title":"/_/clayjs/`+clayruntime.Selected+`/clay.js"}`, nil); w.Code != 400 {
		t.Fatalf("writing a script src = %d, want the 400 that says this engine cannot: %s", w.Code, w.Body.String())
	}

	w := postDataWrite(t, srv, "/_/api/test.htmlclay", `{"text":"board edited"}`, nil)
	if w.Code != 200 {
		t.Fatalf("data write: %d %s", w.Code, w.Body.String())
	}
	assertDiskDocument(t, page, `src="https://clayjs.com/`+clayruntime.Selected+`/clay.js"`)
	disk, err := os.ReadFile(page)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(disk), "board edited") {
		t.Errorf("the data write did not reach disk:\n%s", disk)
	}
}

// A version was written by whatever save preceded it, so restore owes the same canonicalization
// the save lane now performs.
func TestRuntimeCanonicalRestoreWritesHostIndependentBytes(t *testing.T) {
	srv, f, _ := setupHandlerTest(t)
	if w := serve(t, srv, "/test.htmlclay", nil); w.Code != 200 {
		t.Fatalf("serve: %d %s", w.Code, w.Body.String())
	}

	f.Lock()
	key := f.HistoryKey()
	f.Unlock()
	if key == "" {
		t.Fatal("no history key after a serve")
	}
	if _, err := srv.versions.Backup(key, f.AbsPath, []byte(localRuntimeDocument("restored body"))); err != nil {
		t.Fatal(err)
	}
	entries, err := srv.versions.List(key, f.AbsPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) == 0 {
		t.Fatal("the version store lists nothing to restore")
	}
	name := entries[0].Name

	req := httptest.NewRequest("POST", "/_/restore/"+f.Token+"/"+name, nil)
	req.Host = fmt.Sprintf("127.0.0.1:%d", srv.port)
	req.SetPathValue("token", f.Token)
	req.SetPathValue("name", name)
	w := httptest.NewRecorder()
	srv.handleRestoreVersion(w, req)
	if w.Code != 200 {
		t.Fatalf("restore: %d %s", w.Code, w.Body.String())
	}

	assertDiskDocument(t, f.AbsPath, `src="https://clayjs.com/v1/clay.js"`)
	disk, err := os.ReadFile(f.AbsPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(disk), "restored body") {
		t.Errorf("the version's content was not restored:\n%s", disk)
	}
}

// A zip leaves this computer, so its document can never carry a URL that only resolves here.
func TestRuntimeCanonicalExportCarriesPublicURLsOnly(t *testing.T) {
	dir := t.TempDir()
	page := dir + "/board.htmlclay"
	body := localRuntimeDocument("exported")
	if err := os.WriteFile(page, []byte(body), 0644); err != nil {
		t.Fatal(err)
	}

	out, err := ExportDocumentZip(page, t.TempDir(), "")
	if err != nil {
		t.Fatalf("ExportDocumentZip: %v", err)
	}
	entries := zipEntries(t, out)
	document, ok := entries["board/board.htmlclay"]
	if !ok {
		t.Fatalf("the zip has no document: %v", exportNames(entries))
	}
	if !strings.Contains(document, `src="https://clayjs.com/v1/clay.js"`) {
		t.Errorf("the exported document has no public runtime URL:\n%s", document)
	}
	for _, local := range []string{"/_/clayjs/", "clay-runtime-map"} {
		if strings.Contains(document, local) {
			t.Errorf("the exported document kept %q:\n%s", local, document)
		}
	}

	onDisk, err := os.ReadFile(page)
	if err != nil {
		t.Fatal(err)
	}
	if string(onDisk) != body {
		t.Errorf("the export changed the file on disk:\n%s", onDisk)
	}
}

// A relay frame is handed to browsers the way a served document is, and what the relay holds is
// canonical whatever the sender had.
func TestRuntimeCanonicalLiveSyncFrameReachesSubscribersProjected(t *testing.T) {
	srv, f := setupLiveSyncTest(t)
	pageURL := fmt.Sprintf("http://127.0.0.1:%d/page.htmlclay", srv.port)

	for name, snapshot := range map[string]string{
		"local URL": localRuntimeDocument("relayed"),
		"public URL": `<!DOCTYPE html>` + "\n" + `<html lang="en">` + "\n" +
			`<head><script src="https://clayjs.com/` + clayruntime.Selected + `/clay.js"></script></head>` + "\n" +
			`<body><p>relayed</p></body>` + "\n" + `</html>`,
	} {
		t.Run(name, func(t *testing.T) {
			sub := newSubscriber(f.AbsPath, laneLive)
			srv.hub.add(sub)
			t.Cleanup(func() { srv.hub.remove(sub) })

			payload := `{"snapshot":` + quoteJSONString(snapshot) + `,"sender":"c1"}`
			if w := postLiveSync(t, srv, pageURL, payload); w.Code != 200 {
				t.Fatalf("relay: %d %s", w.Code, w.Body.String())
			}
			html, _ := waitFrame(t, sub, time.Second)["html"].(string)
			if !strings.Contains(html, "/_/clayjs/"+clayruntime.Selected+"/clay.js") {
				t.Errorf("the frame was not projected:\n%s", html)
			}
			// A frame carries no descriptor: the receiving tab already holds one, and an
			// inserted element would shift live sync's identity map.
			if strings.Contains(html, `id="clay-runtime-map"`) {
				t.Errorf("the projected frame carries a descriptor:\n%s", html)
			}
			if !strings.Contains(html, "relayed") {
				t.Errorf("the frame lost its content:\n%s", html)
			}
		})
	}
}

// quoteJSONString encodes a document as a JSON string literal.
func quoteJSONString(value string) string {
	replacer := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`, "\r", `\r`, "\t", `\t`)
	return `"` + replacer.Replace(value) + `"`
}

func TestCanonicalForWriteWithoutHostReadsTheLoopbackOrigin(t *testing.T) {
	for _, origin := range []string{"http://127.0.0.1:55600", "http://localhost:55600"} {
		in := `<script src="` + origin + `/_/clayjs/` + clayruntime.Selected + `/clay.js"></script>`
		got := string(canonicalForWrite("", []byte(in)))
		want := `<script src="https://clayjs.com/` + clayruntime.Selected + `/clay.js"></script>`
		if got != want {
			t.Fatalf("canonicalForWrite(%q) = %q, want %q", in, got, want)
		}
	}
}

package server

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/panphora/htmlclay/internal/clayruntime"
	"github.com/panphora/htmlclay/internal/session"
)

// offlineReleaseDir is the vendored release on disk, the tree a browser's edit boot
// would load. It is checked against the embedded index first, so a script reading one
// tree and a server serving another cannot pass each other's work off as agreement.
func offlineReleaseDir(t *testing.T) string {
	t.Helper()
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(cwd, "..", "clayruntime", "assets", clayruntime.Selected)
	bytes, err := os.ReadFile(filepath.Join(dir, "offline-manifest.json"))
	if err != nil {
		t.Fatalf("the release %s is not on disk to read: %v", clayruntime.Selected, err)
	}
	var disk clayruntime.Release
	if err := json.Unmarshal(bytes, &disk); err != nil {
		t.Fatalf("%s/offline-manifest.json: %v", dir, err)
	}
	embedded := clayruntime.Releases[clayruntime.Selected]
	if disk.Version != embedded.Version || len(disk.Files) != len(embedded.Files) {
		t.Fatalf("the disk release is %q with %d files, the embedded one %q with %d",
			disk.Version, len(disk.Files), embedded.Version, len(embedded.Files))
	}
	return dir
}

type editSet struct {
	Files   []string `json:"files"`
	Missing []string `json:"missing"`
	Plugins []string `json:"plugins"`
}

// editBootSet asks the release itself which files an edit boot of a query loads. The
// plan comes from the release's JavaScript, never from a copy of it in Go.
func editBootSet(t *testing.T, node, script, release, query string) editSet {
	t.Helper()
	cmd := exec.Command(node, "--no-warnings", script, release, query)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("%s: %v: %s", query, err, stderr.String())
	}
	var set editSet
	if err := json.Unmarshal(out, &set); err != nil {
		t.Fatalf("%s printed %q: %v", query, out, err)
	}
	return set
}

// A switch to edit reloads into an edit boot, and every file that boot imports must come
// from this host's embedded release: a single 404 is a document that cannot be edited
// offline. The file list is the release's own load plan, resolved by its loader.
func TestOfflineModeEditSetIsServed(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		if os.Getenv("CI") != "" {
			t.Fatal("node is needed to read the release's own load plan")
		}
		t.Skip("node is needed to read the release's own load plan")
	}
	srv, _, _ := setupHandlerTest(t)
	release := offlineReleaseDir(t)
	script := filepath.Join(release, "..", "..", "..", "server", "testdata", "offline-mode", "edit-set.mjs")
	if _, err := os.Stat(script); err != nil {
		t.Fatalf("the load-plan script is missing: %v", err)
	}

	manifestBytes, err := os.ReadFile(filepath.Join(release, "m", "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var manifest struct {
		Runtime string            `json:"runtime"`
		Entries map[string]string `json:"entries"`
	}
	if err := json.Unmarshal(manifestBytes, &manifest); err != nil {
		t.Fatalf("m/manifest.json: %v", err)
	}
	if manifest.Runtime == "" {
		t.Fatal("m/manifest.json has no runtime entry")
	}
	pageChunks := make([]string, 0, 4)
	pageChunks = append(pageChunks, "m/"+manifest.Runtime)
	for _, logical := range []string{"page/editor.js", "core/mode-lifecycle.js", "loader.js"} {
		chunk, ok := manifest.Entries[logical]
		if !ok {
			t.Fatalf("m/manifest.json has no entry for %s", logical)
		}
		pageChunks = append(pageChunks, "m/"+chunk)
	}

	files := clayruntime.Releases[clayruntime.Selected].Files
	queries := []string{
		"plugins=page",
		"plugins=sortable,people,upload,sync,ai-edit",
		"plugins=sync,indicator,ai-edit&exclude=richclay",
	}
	for _, query := range queries {
		set := editBootSet(t, node, script, release, query)
		if len(set.Missing) != 0 {
			t.Fatalf("%s needs %d files the release has no chunk for: %v", query, len(set.Missing), set.Missing)
		}
		if len(set.Files) <= 50 {
			t.Fatalf("%s resolves to %d files, want the whole edit boot", query, len(set.Files))
		}
		t.Logf("%s: %d files", query, len(set.Files))

		if query == "plugins=page" {
			served := make(map[string]bool, len(set.Files))
			for _, name := range set.Files {
				served[name] = true
			}
			for _, chunk := range pageChunks {
				if !served[chunk] {
					t.Errorf("the page edit boot does not load %s", chunk)
				}
			}
		}

		for _, name := range set.Files {
			entry, ok := files[name]
			if !ok {
				t.Errorf("%s names %s, which the release index does not list", query, name)
				continue
			}
			w := runtimeRequest(t, srv, "GET", runtimeAssetTarget(name), nil)
			if w.Code != 200 {
				t.Errorf("%s: %s = %d", query, name, w.Code)
				continue
			}
			sum := sha256.Sum256(w.Body.Bytes())
			if got := hex.EncodeToString(sum[:]); got != entry.SHA256 {
				t.Errorf("%s: %s served %s, want %s", query, name, got, entry.SHA256)
			}
		}
	}
}

// The first edit saves canonical bytes, and the reopened document is projected again under
// the stamp the save answered with, so a second edit does not start from a stale document.
func TestOfflineModeFirstEditSaveAndReopen(t *testing.T) {
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
	saved, ok := decodeJSON(t, w)["etag"].(string)
	if !ok || saved == "" {
		t.Fatalf("the save answered without an etag: %s", w.Body.String())
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
	if !strings.Contains(string(disk), "board edited") {
		t.Errorf("the saved document lost the edit:\n%s", disk)
	}

	again := serve(t, srv, "/kanban.htmlclay", nil)
	if again.Code != 200 {
		t.Fatalf("serve after the save: %d %s", again.Code, again.Body.String())
	}
	reopened := again.Body.String()
	if !strings.Contains(reopened, "/_/clayjs/"+clayruntime.Selected+"/clay.js") ||
		!strings.Contains(reopened, `id="clay-runtime-map"`) {
		t.Errorf("the reopened document was not projected:\n%s", reopened)
	}
	if !strings.Contains(reopened, "board edited") {
		t.Errorf("the reopened document lost the edit:\n%s", reopened)
	}
	if got := decodedDocumentETag(t, again.Body.Bytes()); got != saved {
		t.Errorf("the reopened documentetag = %q, want %q (the etag the save answered with)", got, saved)
	}
}

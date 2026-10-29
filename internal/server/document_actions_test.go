package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// postActionRoutes names the three actions and the route each advertises, so a
// test cannot drift from the URL the meta answer hands a page.
var postActionRoutes = []string{"/_/reveal/", "/_/versions-folder/", "/_/export/"}

// allDocumentActionHooks wires every file action, recording the path each one was
// called with, and fails the test if the host calls one it was not asked for.
func allDocumentActionHooks(t *testing.T, calls map[string][]string) Hooks {
	t.Helper()
	record := func(name string, err error) func(string) error {
		return func(path string) error {
			calls[name] = append(calls[name], path)
			return err
		}
	}
	return Hooks{
		Reveal:         record("reveal", nil),
		OpenFolder:     record("versions", nil),
		ExportDocument: record("export", nil),
	}
}

func postActionThroughMux(t *testing.T, srv *Server, target string, sameOrigin bool) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("POST", target, nil)
	req.Host = fmt.Sprintf("127.0.0.1:%d", srv.port)
	if sameOrigin {
		sameOriginHeaders(req)
	} else {
		req.Header.Set("Sec-Fetch-Site", "cross-site")
	}
	w := httptest.NewRecorder()
	srv.httpServer.Handler.ServeHTTP(w, req)
	return w
}

// documentActionsFromMeta reads document.actions out of the token metadata
// answer, the way the page menu does. A nil map means the block is absent, which
// is what a host with no file actions says.
func documentActionsFromMeta(t *testing.T, body []byte) map[string]string {
	t.Helper()
	var out struct {
		Document *struct {
			Actions map[string]struct {
				Post string `json:"post"`
			} `json:"actions"`
		} `json:"document"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode meta %q: %v", body, err)
	}
	if out.Document == nil {
		t.Fatalf("meta carries no document block: %q", body)
	}
	actions := make(map[string]string, len(out.Document.Actions))
	for name, action := range out.Document.Actions {
		actions[name] = action.Post
	}
	return actions
}

// The page menu learns where to send each action from the document block, so the
// addresses are checked there rather than assumed, and each one carries THIS
// document's token rather than a bare path.
func TestDocumentActionsNamedInMeta(t *testing.T) {
	srv, f, _ := setupHandlerTest(t)
	srv.SetHooks(allDocumentActionHooks(t, map[string][]string{}))

	meta := getThroughMux(t, srv, "/_/meta/"+f.Token)
	if meta.Code != 200 {
		t.Fatalf("meta: got %d, want 200 (%q)", meta.Code, meta.Body.String())
	}
	actions := documentActionsFromMeta(t, meta.Body.Bytes())

	want := map[string]string{
		"reveal":   "/_/reveal/" + f.Token,
		"versions": "/_/versions-folder/" + f.Token,
		"export":   "/_/export/" + f.Token,
	}
	for name, url := range want {
		if actions[name] != url {
			t.Errorf("actions.%s: got %q, want %q (%q)", name, actions[name], url, meta.Body.String())
		}
	}
	if len(actions) != len(want) {
		t.Errorf("actions names %v, want exactly %v", actions, want)
	}
}

// A host with no file-manager or export hook offers no actions at all: naming
// routes that would only refuse would put three dead entries in a person's menu.
func TestDocumentActionsAbsentWithoutHooks(t *testing.T) {
	srv, f, _ := setupHandlerTest(t)

	meta := getThroughMux(t, srv, "/_/meta/"+f.Token)
	if meta.Code != 200 {
		t.Fatalf("meta: got %d, want 200 (%q)", meta.Code, meta.Body.String())
	}
	if raw := meta.Body.String(); strings.Contains(raw, "\"actions\"") {
		t.Errorf("a host with no file action hooks advertises some: %q", raw)
	}
}

// Each action reaches its hook with a path the server chose, never one the page
// sent, and answers ok. The versions action opens the folder this document's
// versions are really written to.
func TestDocumentActionPOSTsReachTheirHook(t *testing.T) {
	for _, route := range postActionRoutes {
		t.Run(strings.Trim(route, "/_"), func(t *testing.T) {
			srv, f, content := setupHandlerTest(t)
			calls := map[string][]string{}
			srv.SetHooks(allDocumentActionHooks(t, calls))

			// A save is what makes a history folder, so the versions action has a
			// real one to open rather than an empty folder created on demand.
			edited := strings.Replace(content, "Hello", "Goodbye", 1)
			if w := saveThroughMux(t, srv, f, edited, ""); w.Code != 200 {
				t.Fatalf("save: got %d, want 200 (%q)", w.Code, w.Body.String())
			}

			w := postActionThroughMux(t, srv, route+f.Token, true)
			if w.Code != 200 {
				t.Fatalf("%s: got %d, want 200 (%q)", route, w.Code, w.Body.String())
			}
			if body := strings.TrimSpace(w.Body.String()); body != `{"ok":true}` {
				t.Errorf("%s reply: got %q, want {\"ok\":true}", route, body)
			}

			var name string
			switch route {
			case "/_/reveal/":
				name = "reveal"
			case "/_/versions-folder/":
				name = "versions"
			case "/_/export/":
				name = "export"
			}
			got := calls[name]
			if len(got) != 1 {
				t.Fatalf("%s called its hook %d times, want 1 (%v)", route, len(got), calls)
			}
			if name == "versions" {
				d, err := os.Stat(got[0])
				if err != nil || !d.IsDir() {
					t.Fatalf("versions action opened %q, want a directory: %v", got[0], err)
				}
				if !strings.HasPrefix(got[0], srv.versions.BaseDir()+string(os.PathSeparator)) {
					t.Errorf("versions folder %q is not under %q", got[0], srv.versions.BaseDir())
				}
				if filepath.Dir(got[0]) != srv.versions.BaseDir() {
					t.Errorf("versions folder %q is not directly under %q", got[0], srv.versions.BaseDir())
				}
				return
			}
			if got[0] != f.AbsPath {
				t.Errorf("%s: hook got %q, want the document's path %q", route, got[0], f.AbsPath)
			}
		})
	}
}

// The page shows whatever sentence comes back, so a failed action must be a
// refusal the page can read rather than an empty 500.
func TestDocumentActionHookErrorIsRefused(t *testing.T) {
	want := map[string]string{
		"/_/reveal/":          "Couldn't show the file.",
		"/_/versions-folder/": "Couldn't open the versions folder.",
		"/_/export/":          "Couldn't export the document.",
	}
	for _, route := range postActionRoutes {
		t.Run(strings.Trim(route, "/_"), func(t *testing.T) {
			srv, f, _ := setupHandlerTest(t)
			broken := func(string) error { return fmt.Errorf("the OS refused") }
			srv.SetHooks(Hooks{Reveal: broken, OpenFolder: broken, ExportDocument: broken})

			w := postActionThroughMux(t, srv, route+f.Token, true)
			if w.Code != 500 {
				t.Fatalf("%s: got %d, want 500 (%q)", route, w.Code, w.Body.String())
			}
			var reply struct {
				OK      bool   `json:"ok"`
				Message string `json:"message"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &reply); err != nil {
				t.Fatalf("%s reply is not JSON: %v (%q)", route, err, w.Body.String())
			}
			if reply.OK {
				t.Errorf("%s: reply says ok on a failed action: %q", route, w.Body.String())
			}
			if reply.Message != want[route] {
				t.Errorf("%s message: got %q, want %q", route, reply.Message, want[route])
			}
		})
	}
}

// These routes make the host touch the person's computer, so the browser's own
// attestation is the gate: a page from anywhere else never reaches a hook.
func TestDocumentActionRefusesCrossSite(t *testing.T) {
	srv, f, _ := setupHandlerTest(t)
	calls := map[string][]string{}
	srv.SetHooks(allDocumentActionHooks(t, calls))

	for _, route := range postActionRoutes {
		w := postActionThroughMux(t, srv, route+f.Token, false)
		if w.Code != http.StatusForbidden {
			t.Errorf("%s cross-site: got %d, want 403 (%q)", route, w.Code, w.Body.String())
		}
	}
	if len(calls) != 0 {
		t.Errorf("a cross-site POST reached the hooks: %v", calls)
	}
}

func TestDocumentActionRefusesUnknownToken(t *testing.T) {
	srv, _, _ := setupHandlerTest(t)
	calls := map[string][]string{}
	srv.SetHooks(allDocumentActionHooks(t, calls))

	for _, route := range postActionRoutes {
		w := postActionThroughMux(t, srv, route+"not-a-token", true)
		if w.Code != http.StatusUnauthorized {
			t.Errorf("%s with an unknown token: got %d, want 401 (%q)", route, w.Code, w.Body.String())
		}
	}
	if len(calls) != 0 {
		t.Errorf("an unknown token reached the hooks: %v", calls)
	}
}

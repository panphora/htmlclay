package server

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/panphora/htmlclay/internal/clayruntime"
	"github.com/panphora/htmlclay/internal/pageadmission"
	"github.com/panphora/htmlclay/internal/session"
	"github.com/panphora/htmlclay/internal/versions"
)

func saveWithFormat(t *testing.T, srv *Server, f *session.File, body, capability string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("POST", "/_/save/"+f.Token, strings.NewReader(body))
	req.Host = fmt.Sprintf("127.0.0.1:%d", srv.port)
	sameOriginHeaders(req)
	if capability != "" {
		req.Header.Set(pageadmission.PageFormatHeader, capability)
	}
	w := httptest.NewRecorder()
	srv.httpServer.Handler.ServeHTTP(w, req)
	return w
}

func postLiveSyncWithFormat(t *testing.T, srv *Server, pageURL, body, capability string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("POST", "/_/sync", strings.NewReader(body))
	req.Host = fmt.Sprintf("127.0.0.1:%d", srv.port)
	req.Header.Set("Content-Type", "application/json")
	if pageURL != "" {
		req.Header.Set("Page-URL", pageURL)
	}
	if capability != "" {
		req.Header.Set(pageadmission.PageFormatHeader, capability)
	}
	w := httptest.NewRecorder()
	srv.handleLiveSyncSave(w, req)
	return w
}

// pageStateOnDisk is the sidecar record for the bytes currently stored, if any.
func pageStateOnDisk(t *testing.T, srv *Server, f *session.File, disk string) (versions.PageStateRecord, bool) {
	t.Helper()
	f.Lock()
	key := f.HistoryKey()
	f.Unlock()
	if key == "" {
		key = versions.Key(f.AbsPath, []byte(disk))
	}
	record, ok, err := srv.versions.PageStateFor(key, f.AbsPath, versions.Hash([]byte(disk)))
	if err != nil {
		t.Fatalf("reading page state: %v", err)
	}
	return record, ok
}

func assertFormatRefusal(t *testing.T, w *httptest.ResponseRecorder, minimum float64) {
	t.Helper()
	body := decode(t, w)
	if w.Code != http.StatusConflict || body["code"] != pageadmission.PageFormatUpgrade ||
		body["minimum"] != minimum {
		t.Fatalf("refusal = %d %s, want 409 %s with minimum %v",
			w.Code, w.Body.String(), pageadmission.PageFormatUpgrade, minimum)
	}
}

func TestPageFormatSaveRefusesALegacyWriterOfARaisedDocument(t *testing.T) {
	srv, _, _ := setupHandlerTest(t)
	raised := pageadmission.WithPageFormatFloor(pageStateDocument(pageStateYJS("seed"), "text"), 1)
	f := registerPageWithContent(t, srv, "raised-page.htmlclay", raised)

	current := pageStateDocument(pageStateYJS("seed"), "one")
	if w := saveWithFormat(t, srv, f, current, "1"); w.Code != http.StatusOK {
		t.Fatalf("current-capability save = %d %s", w.Code, w.Body.String())
	}
	stored := docString(t, f.AbsPath)
	if !strings.Contains(stored, floorMarker()) || !strings.Contains(stored, "one") {
		t.Fatalf("an admitted save of a raised document lost the marker: %q", stored)
	}
	versionsBefore := listVersionNames(t, srv, f)
	stateBefore, stateFoundBefore := pageStateOnDisk(t, srv, f, stored)

	w := saveWithFormat(t, srv, f, pageStateDocument(pageStateYJS("seed"), "two"), "")
	assertFormatRefusal(t, w, 1)
	if on := docString(t, f.AbsPath); on != stored {
		t.Fatalf("a refused save changed disk: %q", on)
	}
	if after := listVersionNames(t, srv, f); fmt.Sprint(after) != fmt.Sprint(versionsBefore) {
		t.Fatalf("a refused save recorded a version: %v to %v", versionsBefore, after)
	}
	stateAfter, stateFoundAfter := pageStateOnDisk(t, srv, f, stored)
	if stateFoundAfter != stateFoundBefore || fmt.Sprint(stateAfter) != fmt.Sprint(stateBefore) {
		t.Fatalf("a refused save moved the page state: %+v to %+v", stateBefore, stateAfter)
	}
}

func floorMarker() string {
	return `<meta name="` + pageadmission.PageFormatMeta + `" content="1">`
}

func TestPageFormatSaveActivatesTheFloor(t *testing.T) {
	srv, _, _ := setupHandlerTest(t)
	plain := pageDocument("")
	f := registerPageWithContent(t, srv, "plain-page.htmlclay", plain)

	declaring := pageadmission.WithPageFormatFloor(pageDocument("<p>newer</p>"), 1)
	if w := saveWithFormat(t, srv, f, declaring, "1"); w.Code != http.StatusOK {
		t.Fatalf("activating save = %d %s", w.Code, w.Body.String())
	}
	stored := docString(t, f.AbsPath)
	if !strings.Contains(stored, floorMarker()) {
		t.Fatalf("the activated floor was not stored: %q", stored)
	}

	w := saveWithFormat(t, srv, f, declaring, "")
	assertFormatRefusal(t, w, 1)
	if on := docString(t, f.AbsPath); on != stored {
		t.Fatalf("a refused save changed disk: %q", on)
	}
}

func TestPageFormatFloorZeroIsUnchanged(t *testing.T) {
	srv, _, _ := setupHandlerTest(t)
	old := pageDocument("")
	f := registerPageWithContent(t, srv, "floor-zero-page.htmlclay", old)

	next := pageDocument("<p>edited</p>")
	if w := saveWithFormat(t, srv, f, next, ""); w.Code != http.StatusOK {
		t.Fatalf("floor-0 save = %d %s", w.Code, w.Body.String())
	}
	if on := docString(t, f.AbsPath); on != next {
		t.Fatalf("a floor-0 save changed the bytes: %q, want %q", on, next)
	}

	if w := saveWithFormat(t, srv, f, pageDocument("<p>again</p>"), "1"); w.Code != http.StatusOK {
		t.Fatalf("floor-0 save with a capability header = %d %s", w.Code, w.Body.String())
	}
	if on := docString(t, f.AbsPath); on != pageDocument("<p>again</p>") {
		t.Fatalf("a capability header changed a floor-0 write: %q", on)
	}
}

func TestPageFormatDataWriteKeepsTheFloor(t *testing.T) {
	srv, _, _ := setupHandlerTest(t)
	raised := pageadmission.WithPageFormatFloor(dataWriteDoc, 1)
	path := writeHome(t, srv, "test.htmlclay", raised)

	w := postDataWrite(t, srv, "/_/api/test.htmlclay", `{"title":"World"}`, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("data write = %d %s", w.Code, w.Body.String())
	}
	stored := readDoc(t, path)
	if !strings.Contains(stored, floorMarker()) {
		t.Fatalf("a data write removed the floor: %q", stored)
	}
	if !strings.Contains(stored, "World") {
		t.Fatalf("the data write did not land: %q", stored)
	}
}

func TestPageFormatRestoreKeepsTheFloor(t *testing.T) {
	fx := setupFileTest(t, "notes.htmlclay", page("raised"))
	fx.serve(t, "notes.htmlclay")

	key := fx.key(t)
	if _, err := fx.srv.versions.Backup(key, fx.file.AbsPath,
		[]byte(page("before the marker existed"))); err != nil {
		t.Fatal(err)
	}
	older := fx.history(t)[0].Name

	if err := os.WriteFile(fx.file.AbsPath,
		[]byte(pageadmission.WithPageFormatFloor(page("raised"), 1)), 0644); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest("POST", "/_/restore/"+fx.file.Token+"/"+older, nil)
	req.Host = fmt.Sprintf("127.0.0.1:%d", fx.srv.port)
	req.SetPathValue("token", fx.file.Token)
	req.SetPathValue("name", older)
	w := httptest.NewRecorder()
	fx.srv.handleRestoreVersion(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("restore = %d %s", w.Code, w.Body.String())
	}

	stored := docString(t, fx.file.AbsPath)
	if !strings.Contains(stored, "before the marker existed") || !strings.Contains(stored, floorMarker()) {
		t.Fatalf("a restore dropped the floor: %q", stored)
	}
}

func TestPageFormatRelayRefusesALegacyWriter(t *testing.T) {
	srv, f := setupLiveSyncTest(t)
	pageURL := fmt.Sprintf("http://127.0.0.1:%d/page.htmlclay", srv.port)
	if err := os.WriteFile(f.AbsPath,
		[]byte(pageadmission.WithPageFormatFloor(page("hi"), 1)), 0644); err != nil {
		t.Fatal(err)
	}

	sub := newSubscriber(f.AbsPath, laneLive)
	srv.hub.add(sub)

	w := postLiveSyncWithFormat(t, srv, pageURL, `{"snapshot":"<html><body>peer</body></html>","sender":"c1"}`, "")
	assertFormatRefusal(t, w, 1)
	expectNoFrame(t, sub, 200*time.Millisecond)

	w = postLiveSyncWithFormat(t, srv, pageURL, `{"snapshot":"<html><body>peer</body></html>","sender":"c1"}`, "1")
	if w.Code != http.StatusOK {
		t.Fatalf("relay with a current capability = %d %s", w.Code, w.Body.String())
	}
	msg := waitFrame(t, sub, time.Second)
	if msg["sender"] != "c1" || !strings.Contains(fmt.Sprint(msg["html"]), "peer") {
		t.Fatalf("relayed frame is not the peer's: %v", msg)
	}
}

func TestPageFormatMetaAdvertisesOnlyWithAFloorRuntime(t *testing.T) {
	if !clayruntime.SelectedHas("src/core/page-data.js") {
		t.Fatal("the bundled release no longer lists a file it certainly carries")
	}
	floor := clayruntime.SelectedHas("src/core/page-format.js")

	srv, f, _ := setupHandlerTest(t)
	for _, route := range []string{"/_/meta", "/_/meta/" + f.Token} {
		w := getThroughMux(t, srv, route)
		if w.Code != 200 {
			t.Fatalf("%s: got %d (%q)", route, w.Code, w.Body.String())
		}
		announced := false
		for _, name := range metaExtensions(t, w) {
			announced = announced || name == "page-format-v1"
		}
		if announced != floor {
			t.Fatalf("%s: page-format-v1 announced = %v, want %v", route, announced, floor)
		}
	}
}

package server

import (
	"crypto/sha256"
	"encoding/json"
	"net/http/httptest"
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// A real person id shape, so the test would notice an answer that only looked
// like one.
const (
	metaPersonID   = "sV6g2rT9pQ4wX1yZ"
	metaPersonName = "Ada Chen"
)

// rawPeople returns `document.people` exactly as served, so an absent key, a
// null, and a person are three different answers to the test. present is false
// when the document block carries no people key at all.
func rawPeople(t *testing.T, w *httptest.ResponseRecorder) (string, bool) {
	t.Helper()
	var body struct {
		Document *struct {
			People json.RawMessage `json:"people"`
		} `json:"document"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("response is not JSON: %v (%q)", err, w.Body.String())
	}
	if body.Document == nil || body.Document.People == nil {
		return "", false
	}
	return string(body.Document.People), true
}

func metaExtensions(t *testing.T, w *httptest.ResponseRecorder) []string {
	t.Helper()
	var answer struct {
		Extensions []string `json:"extensions"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &answer); err != nil {
		t.Fatalf("response is not JSON: %v (%q)", err, w.Body.String())
	}
	return answer.Extensions
}

// A server with no Profile hook is what every existing test builds, and it must
// keep answering exactly what it answered before People existed: the same
// extension list, and no people key anywhere.
func TestMetaWithoutAProfileNamesNoOne(t *testing.T) {
	srv, f, _ := setupHandlerTest(t)

	// Spelled out rather than read from hostExtensions, because comparing the
	// package against itself would pass the day the list gains a member.
	want := []string{"conditional", "data-read", "data-write", "receipts", "sync", "sync-worker", "upload", "wire"}

	for _, route := range []string{"/_/meta", "/_/meta/" + f.Token} {
		w := getThroughMux(t, srv, route)
		if w.Code != 200 {
			t.Fatalf("%s: got %d, want 200 (%q)", route, w.Code, w.Body.String())
		}
		if got := metaExtensions(t, w); !reflect.DeepEqual(got, want) {
			t.Errorf("%s: extensions = %v, want %v", route, got, want)
		}
		if strings.Contains(w.Body.String(), `"people"`) {
			t.Errorf("%s: a server with no Profile hook named people: %q", route, w.Body.String())
		}
	}

	if _, present := rawPeople(t, getThroughMux(t, srv, "/_/meta/"+f.Token)); present {
		t.Error("a server with no Profile hook answered with a people key")
	}
}

func TestMetaWithAProfileNamesThePerson(t *testing.T) {
	srv, f, _ := setupHandlerTest(t)
	srv.SetHooks(Hooks{Profile: func() (string, string, bool) { return metaPersonID, metaPersonName, true }})

	w := getThroughMux(t, srv, "/_/meta/"+f.Token)
	if w.Code != 200 {
		t.Fatalf("got %d, want 200 (%q)", w.Code, w.Body.String())
	}

	extensions := metaExtensions(t, w)
	if !sort.StringsAreSorted(extensions) {
		t.Errorf("extensions are not sorted: %v", extensions)
	}
	announced := false
	for _, name := range extensions {
		announced = announced || name == "people"
	}
	if !announced {
		t.Fatalf("a host with a Profile hook does not announce people: %v", extensions)
	}

	got, present := rawPeople(t, w)
	want := `{"me":{"id":"` + metaPersonID + `","name":"` + metaPersonName + `"}}`
	if !present || got != want {
		t.Errorf("people = %s (present %v), want %s", got, present, want)
	}
	if got := w.Header().Get("Cache-Control"); got != "private, no-store" {
		t.Errorf("Cache-Control = %q, want %q", got, "private, no-store")
	}
	if strings.Contains(w.Body.String(), "members") {
		t.Errorf("the answer carries a members field: %q", w.Body.String())
	}
}

// Sharing off is not the same as no People at all: the host still announces the
// extension and still withholds the answer from a shared cache, and the document
// is told `me: null` rather than being told nothing.
func TestMetaWithSharingOffAnswersMeNull(t *testing.T) {
	srv, f, _ := setupHandlerTest(t)
	srv.SetHooks(Hooks{Profile: func() (string, string, bool) { return "", "", false }})

	w := getThroughMux(t, srv, "/_/meta/"+f.Token)
	if w.Code != 200 {
		t.Fatalf("got %d, want 200 (%q)", w.Code, w.Body.String())
	}
	got, present := rawPeople(t, w)
	if !present || got != `{"me":null}` {
		t.Errorf("people = %s (present %v), want {\"me\":null}", got, present)
	}
	if got := w.Header().Get("Cache-Control"); got != "private, no-store" {
		t.Errorf("Cache-Control = %q, want %q", got, "private, no-store")
	}
	if !strings.Contains(w.Body.String(), `"people"`) {
		t.Errorf("a host that offers People stopped announcing it when sharing was turned off: %q", w.Body.String())
	}
}

// The tokenless route names no document, so it names no person either: it may
// say the capability exists, and nothing about who is asking.
func TestHostMetaWithAProfileNamesNoOne(t *testing.T) {
	srv, _, _ := setupHandlerTest(t)
	srv.SetHooks(Hooks{Profile: func() (string, string, bool) { return metaPersonID, metaPersonName, true }})

	w := getThroughMux(t, srv, "/_/meta")
	if w.Code != 200 {
		t.Fatalf("got %d, want 200 (%q)", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, `"people"`) {
		t.Errorf("a host with a Profile hook does not announce people: %q", body)
	}

	var answer map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &answer); err != nil {
		t.Fatal(err)
	}
	if _, present := answer["document"]; present {
		t.Errorf("the tokenless route described a document: %q", body)
	}
	if strings.Contains(body, metaPersonID) || strings.Contains(body, metaPersonName) {
		t.Errorf("the tokenless route named the person: %q", body)
	}
}

// A caller who may not ask about a document is told nothing about it, and the
// person is per-document: the refusal is what it always was, and it names no one.
func TestMetaWithABadOrMissingTokenNamesNoOne(t *testing.T) {
	srv, _, _ := setupHandlerTest(t)
	srv.SetHooks(Hooks{Profile: func() (string, string, bool) { return metaPersonID, metaPersonName, true }})

	// 401 is the token handleMeta does not hold. 403 is the read refusal a bare
	// trailing slash falls to: a single wildcard does not match an empty segment,
	// so "/_/meta/" reaches the file catch-all rather than the meta route.
	for _, tc := range []struct {
		route  string
		status int
	}{
		{"/_/meta/not-a-real-token", 401},
		{"/_/meta/", 403},
	} {
		w := getThroughMux(t, srv, tc.route)
		if w.Code != tc.status {
			t.Errorf("%s: got %d, want %d (%q)", tc.route, w.Code, tc.status, w.Body.String())
		}
		if strings.Contains(w.Body.String(), metaPersonID) || strings.Contains(w.Body.String(), metaPersonName) {
			t.Errorf("%s: the refusal named the person: %q", tc.route, w.Body.String())
		}
	}
}

// The hook is read on every answer, so turning sharing on, off, or renaming the
// person reaches the documents already open.
func TestMetaReadsTheProfileLive(t *testing.T) {
	srv, f, _ := setupHandlerTest(t)
	id, name := metaPersonID, metaPersonName
	srv.SetHooks(Hooks{Profile: func() (string, string, bool) { return id, name, true }})

	first, _ := rawPeople(t, getThroughMux(t, srv, "/_/meta/"+f.Token))
	if first != `{"me":{"id":"`+id+`","name":"`+metaPersonName+`"}}` {
		t.Fatalf("first answer = %s", first)
	}

	name = "Ada Lovelace"
	second, _ := rawPeople(t, getThroughMux(t, srv, "/_/meta/"+f.Token))
	want := `{"me":{"id":"` + id + `","name":"Ada Lovelace"}}`
	if second != want {
		t.Errorf("second answer = %s, want %s", second, want)
	}
}

// Discovery is a read of the document's identity, never a write to the file.
func TestMetaWritesNothingToTheDocument(t *testing.T) {
	srv, f, _ := setupHandlerTest(t)

	before, err := os.ReadFile(f.AbsPath)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(before)

	on := true
	name := metaPersonName
	srv.SetHooks(Hooks{Profile: func() (string, string, bool) { return metaPersonID, name, on }})

	for i := 0; i < 3; i++ {
		switch i {
		case 1:
			on = false
		case 2:
			on = true
			name = "Ada Lovelace"
		}
		if w := getThroughMux(t, srv, "/_/meta/"+f.Token); w.Code != 200 {
			t.Fatalf("meta request %d: got %d (%q)", i, w.Code, w.Body.String())
		}
	}

	after, err := os.ReadFile(f.AbsPath)
	if err != nil {
		t.Fatal(err)
	}
	if sha256.Sum256(after) != digest {
		t.Errorf("the document on disk changed: %d bytes before, %d after", len(before), len(after))
	}
}

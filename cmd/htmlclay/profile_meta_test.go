package main

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/panphora/htmlclay/internal/config"
	"github.com/panphora/htmlclay/internal/platform"
)

// metaPeopleOf reads `document.people` through the real route, so what a
// document would see is what is asserted, and reports the Cache-Control the
// answer was served with.
func metaPeopleOf(t *testing.T, s *site, document string) (string, string) {
	t.Helper()
	f, ok := s.sessions.LookupByPath(document)
	if !ok {
		t.Fatalf("%s is not registered in any live site", document)
	}
	status, headers, body := fetchFull(t, fmt.Sprintf("http://127.0.0.1:%d/_/meta/%s", s.port, f.Token))
	if status != 200 {
		t.Fatalf("meta status %d: %s", status, body)
	}
	var answer struct {
		Document struct {
			People json.RawMessage `json:"people"`
		} `json:"document"`
	}
	if err := json.Unmarshal([]byte(body), &answer); err != nil {
		t.Fatal(err)
	}
	return string(answer.Document.People), headers.Get("Cache-Control")
}

func profilePerson(id, name string) string {
	return `{"me":{"id":"` + id + `","name":"` + name + `"}}`
}

// One person, not one per origin: two files in two unrelated trees anchor on
// different sites and different ports, and both must name the same person.
func TestProfileMetaServesOnePersonAcrossOrigins(t *testing.T) {
	home, err := resolveSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	a := newTestApp(t, home)
	id, err := config.NewProfileID()
	if err != nil {
		t.Fatal(err)
	}
	if err := a.rt.cfg.UpdateProfile(config.Profile{Enabled: true, ID: id, Name: "Ada Chen"}); err != nil {
		t.Fatal(err)
	}

	first := filepath.Join(home, "projA", "index.htmlclay")
	second := filepath.Join(home, "projB", "index.htmlclay")
	writeTestFile(t, first, "<html><body>alpha</body></html>")
	writeTestFile(t, second, "<html><body>bravo</body></html>")

	siteA, _ := a.openForTest(t, first)
	siteB, _ := a.openForTest(t, second)
	if siteA.port == siteB.port {
		t.Fatalf("both files landed on port %d; this test needs two origins", siteA.port)
	}

	for _, s := range []struct {
		site     *site
		document string
	}{{siteA, first}, {siteB, second}} {
		got, cache := metaPeopleOf(t, s.site, s.document)
		if got != profilePerson(id, "Ada Chen") {
			t.Errorf("origin %d: people = %s, want %s", s.site.port, got, profilePerson(id, "Ada Chen"))
		}
		if cache != "private, no-store" {
			t.Errorf("origin %d: Cache-Control = %q, want %q", s.site.port, cache, "private, no-store")
		}
	}
}

// The id is the person's identity across launches: a config saved by one app and
// loaded by the next must answer with the same id.
func TestProfileMetaKeepsTheIDAcrossARestart(t *testing.T) {
	home, err := resolveSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cfgBase := t.TempDir()
	a := newTestAppWithConfigDir(t, home, cfgBase)
	id, err := config.NewProfileID()
	if err != nil {
		t.Fatal(err)
	}
	if err := a.rt.cfg.UpdateProfile(config.Profile{Enabled: true, ID: id, Name: "Ada Chen"}); err != nil {
		t.Fatal(err)
	}

	document := filepath.Join(home, "docs", "page.htmlclay")
	writeTestFile(t, document, "<html><body>hi</body></html>")
	s, _ := a.openForTest(t, document)
	if got, _ := metaPeopleOf(t, s, document); got != profilePerson(id, "Ada Chen") {
		t.Fatalf("before the restart: people = %s", got)
	}

	reloaded, _, err := config.LoadFrom(cfgBase, platform.DirIdentity)
	if err != nil {
		t.Fatal(err)
	}
	if got := reloaded.ProfileState().ID; got != id {
		t.Fatalf("the reloaded config holds id %q, want %q", got, id)
	}

	restarted := newTestAppWithConfigDir(t, home, cfgBase)
	restartedSite, _ := restarted.openForTest(t, document)
	if got, _ := metaPeopleOf(t, restartedSite, document); got != profilePerson(id, "Ada Chen") {
		t.Errorf("after the restart: people = %s, want %s", got, profilePerson(id, "Ada Chen"))
	}
}

// Turning sharing off reaches the files already open, with no restart: the next
// answer is `me: null`.
func TestProfileMetaFollowsSharingOff(t *testing.T) {
	home, err := resolveSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	a := newTestApp(t, home)
	id, err := config.NewProfileID()
	if err != nil {
		t.Fatal(err)
	}
	if err := a.rt.cfg.UpdateProfile(config.Profile{Enabled: true, ID: id, Name: "Ada Chen"}); err != nil {
		t.Fatal(err)
	}

	document := filepath.Join(home, "docs", "page.htmlclay")
	writeTestFile(t, document, "<html><body>hi</body></html>")
	s, _ := a.openForTest(t, document)

	if got, _ := metaPeopleOf(t, s, document); got != profilePerson(id, "Ada Chen") {
		t.Fatalf("sharing on: people = %s", got)
	}

	if err := a.rt.cfg.UpdateProfile(config.Profile{Enabled: false, ID: id, Name: "Ada Chen"}); err != nil {
		t.Fatal(err)
	}
	got, cache := metaPeopleOf(t, s, document)
	if got != `{"me":null}` {
		t.Errorf("sharing off: people = %s, want {\"me\":null}", got)
	}
	if cache != "private, no-store" {
		t.Errorf("sharing off: Cache-Control = %q, want %q", cache, "private, no-store")
	}
}

package main

import (
	"fmt"
	"path/filepath"
	"regexp"
	"testing"

	"github.com/panphora/htmlclay/internal/server"
)

func TestSlugBase(t *testing.T) {
	for in, want := range map[string]string{
		"w4": "w4", "Notes": "notes", "My Project (2026)": "my-project-2026", "日記": "site",
		"--weird--": "weird", "a_b.c": "a-b-c",
		"this-folder-name-is-much-longer-than-the-cap": "this-folder-name-is-much",
	} {
		if got := slugBase(in); got != want {
			t.Errorf("slugBase(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSlugIsStableUniqueAndNeverRetired(t *testing.T) {
	home, _ := resolveSymlinks(t.TempDir())
	a := buildTestApp(t, home, t.TempDir())
	a.mu.Lock()
	first := a.slugForLocked(filepath.Join(home, "notes"), "notes")
	again := a.slugForLocked(filepath.Join(home, "notes"), "notes")
	other := a.slugForLocked(filepath.Join(home, "other", "notes"), "notes")
	a.mu.Unlock()
	if first != again {
		t.Fatalf("a key must keep its slug: %q then %q", first, again)
	}
	if first == other {
		t.Fatalf("two keys must not share a slug: %q", first)
	}
	if !regexp.MustCompile(`^notes-[0-9a-f]{4}$`).MatchString(first) || !server.ValidSlug(first) {
		t.Fatalf("slug shape: %q", first)
	}
	retired := a.rt.cfg.ForgetSiteSlug(filepath.Join(home, "notes"))
	for i := 0; i < 200; i++ {
		a.mu.Lock()
		got := a.slugForLocked(filepath.Join(home, fmt.Sprintf("n%d", i), "notes"), "notes")
		a.mu.Unlock()
		if got == retired {
			t.Fatalf("a retired slug was reissued: %q", got)
		}
	}
}

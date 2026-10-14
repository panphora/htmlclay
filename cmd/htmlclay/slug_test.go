package main

import (
	"path/filepath"
	"testing"

	"github.com/panphora/htmlclay/internal/server"
)

func TestSlugIsStableUniqueAndNeverRetired(t *testing.T) {
	home, _ := resolveSymlinks(t.TempDir())
	a := buildTestApp(t, home, t.TempDir())
	firstKey := filepath.Join(home, "notes")
	secondKey := filepath.Join(home, "other", "notes")
	a.mu.Lock()
	first := a.slugForLocked(firstKey)
	again := a.slugForLocked(firstKey)
	other := a.slugForLocked(secondKey)
	a.mu.Unlock()
	if first != "c1" {
		t.Fatalf("the first slug = %q, want c1", first)
	}
	if again != first {
		t.Fatalf("a key must keep its slug: %q then %q", first, again)
	}
	if other != "c2" {
		t.Fatalf("the second key's slug = %q, want c2", other)
	}
	if retired := a.rt.cfg.ForgetSiteSlug(firstKey); retired != "c1" {
		t.Fatalf("ForgetSiteSlug returned %q, want c1", retired)
	}
	a.mu.Lock()
	afterForget := a.slugForLocked(filepath.Join(home, "third"))
	a.mu.Unlock()
	if afterForget != "c3" {
		t.Fatalf("a retired label must never be reissued: got %q, want c3", afterForget)
	}
	a.rt.cfg.RememberSiteSlug("/manual", "c4")
	a.mu.Lock()
	skipped := a.slugForLocked(filepath.Join(home, "fourth"))
	a.mu.Unlock()
	if skipped != "c5" {
		t.Fatalf("a label already issued must be skipped: got %q, want c5", skipped)
	}
	for _, slug := range []string{first, again, other, afterForget, skipped} {
		if !server.ValidSlug(slug) {
			t.Fatalf("slug %q is not a valid label", slug)
		}
	}
}

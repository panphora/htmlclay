package server

import (
	"net/http"
	"path/filepath"
	"testing"

	"github.com/panphora/htmlclay/versions"
)

func TestValidateHostCorrect(t *testing.T) {
	r := &http.Request{Host: "127.0.0.1:49821"}
	if !ValidateHost(r, 49821) {
		t.Error("expected true for 127.0.0.1:49821")
	}
}

func TestValidateHostLocalhost(t *testing.T) {
	r := &http.Request{Host: "localhost:49821"}
	if !ValidateHost(r, 49821) {
		t.Error("expected true for localhost:49821")
	}
}

func TestValidateHostWrongPort(t *testing.T) {
	r := &http.Request{Host: "127.0.0.1:9999"}
	if ValidateHost(r, 49821) {
		t.Error("expected false for wrong port")
	}
}

func TestValidateHostEvil(t *testing.T) {
	r := &http.Request{Host: "evil.com:49821"}
	if ValidateHost(r, 49821) {
		t.Error("expected false for evil.com")
	}
}

func TestValidateHostNoPort(t *testing.T) {
	r := &http.Request{Host: "127.0.0.1"}
	if ValidateHost(r, 49821) {
		t.Error("expected false for missing port")
	}
}

func TestValidateHostEmpty(t *testing.T) {
	r := &http.Request{Host: ""}
	if ValidateHost(r, 49821) {
		t.Error("expected false for empty host")
	}
}

func TestValidatePathSuccess(t *testing.T) {
	home := t.TempDir()
	p, err := ValidatePath("Documents/file.htmlclay", home)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := filepath.Join(home, "Documents", "file.htmlclay")
	if p != want {
		t.Errorf("got %q, want %q", p, want)
	}
}

func TestValidatePathSimple(t *testing.T) {
	home := t.TempDir()
	p, err := ValidatePath("file.htmlclay", home)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := filepath.Join(home, "file.htmlclay")
	if p != want {
		t.Errorf("got %q, want %q", p, want)
	}
}

func TestValidatePathNested(t *testing.T) {
	home := t.TempDir()
	p, err := ValidatePath("a/b/c/file.htmlclay", home)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := filepath.Join(home, "a", "b", "c", "file.htmlclay")
	if p != want {
		t.Errorf("got %q, want %q", p, want)
	}
}

func TestValidatePathTraversal(t *testing.T) {
	_, err := ValidatePath("../../../etc/passwd", "/Users/david")
	if err == nil {
		t.Error("expected error for path traversal")
	}
}

func TestValidatePathTraversalMiddle(t *testing.T) {
	_, err := ValidatePath("Documents/../../etc/passwd", "/Users/david")
	if err == nil {
		t.Error("expected error for mid-path traversal")
	}
}

func TestValidatePathAbsolute(t *testing.T) {
	_, err := ValidatePath("/etc/passwd", "/Users/david")
	if err == nil {
		t.Error("expected error for absolute path")
	}
}

func TestValidatePathNullByte(t *testing.T) {
	_, err := ValidatePath("Documents/file\x00.htmlclay", "/Users/david")
	if err == nil {
		t.Error("expected error for null byte")
	}
}

func TestValidatePathNormalized(t *testing.T) {
	home := t.TempDir()
	p, err := ValidatePath("Documents/../Documents/file.htmlclay", home)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := filepath.Join(home, "Documents", "file.htmlclay")
	if p != want {
		t.Errorf("got %q, want %q", p, want)
	}
}

// A version name is validated as exactly one generated filename after decoding.
// Nothing traversal-shaped, and nothing that names a store-internal file, is
// accepted on any of the token-bearing version routes.
func TestVersionNameValidationRejectsTraversal(t *testing.T) {
	bad := []string{
		"../meta.json",
		"../../../etc/passwd",
		"..%2f..%2fetc%2fpasswd",
		"/etc/passwd",
		"meta.json",
		".htmlclay-ver-123",
		"2026-07-19-14-22-08-431Z.html/../../secret",
		"2026-07-19-14-22-08-431Z.html\x00.png",
		"",
	}
	for _, name := range bad {
		if _, _, err := versions.ParseEntryName(name); err == nil {
			t.Errorf("ParseEntryName accepted %q", name)
		}
	}

	// The generated shape, and only that shape, is accepted.
	for _, good := range []string{
		"2026-07-19-14-22-08-431Z.html",
		"2026-07-19-14-22-08-431Z-02.html",
		"2026-07-19-14-22-08-431Z-100.html",
	} {
		if _, _, err := versions.ParseEntryName(good); err != nil {
			t.Errorf("ParseEntryName rejected a generated name %q: %v", good, err)
		}
	}
}

// The versions directory is internal state. ValidatePath still contains it inside
// home, so the denial is a separate, explicit check rather than a side effect of
// path containment.
func TestVersionsPathIsContainedButStillDenied(t *testing.T) {
	homeDir, _ := filepath.EvalSymlinks(t.TempDir())
	store := versions.New(filepath.Join(homeDir, "versions"))

	abs, err := ValidatePath("versions/notes-abcd1234/2026-07-19-14-22-08-431Z.html", homeDir)
	if err != nil {
		t.Fatalf("ValidatePath rejected an in-home path: %v", err)
	}
	if !store.Contains(abs) {
		t.Fatal("a versions path inside the store was not recognized as internal")
	}
}

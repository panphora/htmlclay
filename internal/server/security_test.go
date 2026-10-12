package server

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/panphora/htmlclay/internal/versions"
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

// The same-origin gate admits only a browser-attested request from this site's
// own page: Sec-Fetch-Site must be literally same-origin (same-site means any
// other loopback port), and Origin, when present, must name this origin, port
// included. Origin cannot be REQUIRED: Chrome omits it on same-origin GETs,
// including EventSource's stream GET — the one GET route the gate wraps — so
// requiring it 403'd every live-sync stream while every POST passed (POSTs
// always carry Origin). Verified live against Chrome 147. Absent Sec-Fetch-Site
// is what an old browser's no-cors request and a bare local process send, and
// both are refused.
func TestSameOriginGate(t *testing.T) {
	handled := false
	h := sameOrigin(func(w http.ResponseWriter, r *http.Request) {
		handled = true
		w.WriteHeader(http.StatusOK)
	})

	cases := []struct {
		name   string
		site   string
		origin string
		want   int
	}{
		{"attested same-origin", "same-origin", "http://127.0.0.1:7777", 200},
		{"same-site is another local port", "same-site", "http://127.0.0.1:7777", 403},
		{"cross-site", "cross-site", "http://evil.example", 403},
		{"none (direct navigation)", "none", "http://127.0.0.1:7777", 403},
		{"absent headers", "", "", 403},
		{"origin names a different port", "same-origin", "http://127.0.0.1:9999", 403},
		{"origin absent (Chrome same-origin GET / EventSource)", "same-origin", "", 200},
		{"origin is https", "same-origin", "https://127.0.0.1:7777", 403},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			handled = false
			req := httptest.NewRequest("POST", "/_/save/tok", nil)
			req.Host = "127.0.0.1:7777"
			if tc.site != "" {
				req.Header.Set("Sec-Fetch-Site", tc.site)
			}
			if tc.origin != "" {
				req.Header.Set("Origin", tc.origin)
			}
			w := httptest.NewRecorder()
			h(w, req)
			if w.Code != tc.want {
				t.Fatalf("status = %d, want %d", w.Code, tc.want)
			}
			if handled != (tc.want == 200) {
				t.Fatalf("handler ran = %v with status %d", handled, w.Code)
			}
		})
	}
}

func TestHostGateAllowsOnlyNamedHosts(t *testing.T) {
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) })
	ours := func(h string) bool { return IsSiteHost(h, 24190) || h == "127.0.0.1:51000" }
	gate := HostGate(ok, ours, "w4-3f2a.localhost:24190")
	cases := []struct {
		name, host, site, mode, dest, referer string
		want                                  int
	}{
		{"own host", "w4-3f2a.localhost:24190", "", "", "", "", 204},
		{"other slug", "notes-9c1e.localhost:24190", "", "", "", "", 403},
		{"other port", "w4-3f2a.localhost:24191", "", "", "", "", 403},
		{"bare loopback", "127.0.0.1:24190", "", "", "", "", 403},
		{"uppercase is not canonical", "W4-3F2A.localhost:24190", "", "", "", "", 403},
		{"cross-site fetch", "w4-3f2a.localhost:24190", "cross-site", "cors", "empty", "http://notes-9c1e.localhost:24190/", 403},
		{"cross-site navigation from a sibling", "w4-3f2a.localhost:24190", "cross-site", "navigate", "document", "http://notes-9c1e.localhost:24190/a.htmlclay", 204},
		{"cross-site navigation from a sibling, origin-only referer", "w4-3f2a.localhost:24190", "cross-site", "navigate", "document", "http://notes-9c1e.localhost:24190/", 204},
		{"cross-site navigation from a held legacy port", "w4-3f2a.localhost:24190", "cross-site", "navigate", "document", "http://127.0.0.1:51000/", 204},
		{"cross-site navigation from the web", "w4-3f2a.localhost:24190", "cross-site", "navigate", "document", "https://evil.example/", 403},
		{"cross-site navigation from another port", "w4-3f2a.localhost:24190", "cross-site", "navigate", "document", "http://notes-9c1e.localhost:3000/", 403},
		{"cross-site navigation with no referer", "w4-3f2a.localhost:24190", "cross-site", "navigate", "document", "", 403},
		{"cross-site iframe from a sibling", "w4-3f2a.localhost:24190", "cross-site", "navigate", "iframe", "http://notes-9c1e.localhost:24190/", 403},
		{"same-site fetch stays allowed", "w4-3f2a.localhost:24190", "same-site", "no-cors", "image", "", 204},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/x.htmlclay", nil)
			req.Host = tc.host
			for k, v := range map[string]string{"Sec-Fetch-Site": tc.site, "Sec-Fetch-Mode": tc.mode, "Sec-Fetch-Dest": tc.dest, "Referer": tc.referer} {
				if v != "" {
					req.Header.Set(k, v)
				}
			}
			w := httptest.NewRecorder()
			gate.ServeHTTP(w, req)
			if w.Code != tc.want {
				t.Fatalf("got %d, want %d", w.Code, tc.want)
			}
		})
	}
}

func TestEligibleNavigation(t *testing.T) {
	ours := func(h string) bool { return h == "a-1111.localhost:24190" }
	nav := func(site, user, referer string) *http.Request {
		r := httptest.NewRequest(http.MethodGet, "/p.htmlclay", nil)
		r.Header.Set("Sec-Fetch-Mode", "navigate")
		r.Header.Set("Sec-Fetch-Dest", "document")
		if site != "" {
			r.Header.Set("Sec-Fetch-Site", site)
		}
		if user != "" {
			r.Header.Set("Sec-Fetch-User", user)
		}
		if referer != "" {
			r.Header.Set("Referer", referer)
		}
		return r
	}
	cases := map[string]struct {
		r    *http.Request
		want bool
	}{
		"typed":                     {nav("none", "", ""), true},
		"same-origin user":          {nav("same-origin", "?1", ""), true},
		"same-origin script":        {nav("same-origin", "", ""), false},
		"cross-site from ours":      {nav("cross-site", "", "http://a-1111.localhost:24190/"), true},
		"cross-site uppercase ours": {nav("cross-site", "", "http://A-1111.localhost:24190/"), true},
		"same-site from ours":       {nav("same-site", "", "http://a-1111.localhost:24190/x"), true},
		"cross-site from the web":   {nav("cross-site", "", "https://evil.example/"), false},
		"cross-site no referer":     {nav("cross-site", "", ""), false},
		"no fetch metadata":         {nav("", "", ""), false},
		"not a navigation (iframe)": {func() *http.Request { r := nav("none", "", ""); r.Header.Set("Sec-Fetch-Dest", "iframe"); return r }(), false},
		"not a navigation (POST)":   {func() *http.Request { r := nav("none", "", ""); r.Method = http.MethodPost; return r }(), false},
	}
	for name, tc := range cases {
		if got := EligibleNavigation(tc.r, ours); got != tc.want {
			t.Errorf("%s: %v, want %v", name, got, tc.want)
		}
	}
}

func TestValidSlug(t *testing.T) {
	for slug, want := range map[string]bool{"w4-3f2a": true, "notes-9c1e": true, "a": true, "9lives": true,
		"-a": false, "a-": false, "A-1": false, "a.b": false, "": false, "a_b": false} {
		if got := ValidSlug(slug); got != want {
			t.Errorf("ValidSlug(%q) = %v, want %v", slug, got, want)
		}
	}
}

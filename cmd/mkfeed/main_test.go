package main

import (
	"bytes"
	"compress/gzip"
	"crypto/ed25519"
	"crypto/rand"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/panphora/htmlclay/internal/update"
)

const (
	testVersion = "1.14.0"
	testSHA     = "0123456789abcdef0123456789abcdef01234567"
	testDate    = "2026-10-14T18:30:00Z"
)

// writeArtifacts lays out what the release workflow downloads into updates/:
// the Windows exes as they are and the Linux binaries gzipped.
func writeArtifacts(t *testing.T, dir string) map[string][]byte {
	t.Helper()
	binaries := map[string][]byte{}
	for _, target := range targets {
		bin := []byte("binary for " + target)
		binaries[target] = bin
		data := bin
		if strings.HasPrefix(target, "linux-") {
			var buf bytes.Buffer
			gz := gzip.NewWriter(&buf)
			gz.Write(bin)
			gz.Close()
			data = buf.Bytes()
		}
		if err := os.WriteFile(filepath.Join(dir, artifactName(testVersion, target)), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return binaries
}

func signedFeed(t *testing.T, payload []byte) ([]byte, map[string]ed25519.PublicKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	feed, err := update.SignPayload(payload, "test-key", priv)
	if err != nil {
		t.Fatal(err)
	}
	return feed, map[string]ed25519.PublicKey{"test-key": pub}
}

func TestPayloadVerifiesAndEveryArtifactDownloadsAndStages(t *testing.T) {
	dir := t.TempDir()
	binaries := writeArtifacts(t, dir)

	srv := httptest.NewTLSServer(http.StripPrefix("/update/"+testVersion+"/", http.FileServer(http.Dir(dir))))
	defer srv.Close()

	payload, err := buildPayload(dir, testVersion, testSHA, srv.URL+"/update/", testDate)
	if err != nil {
		t.Fatal(err)
	}
	feed, keys := signedFeed(t, payload)
	if err := checkFeed(feed, testVersion, keys); err != nil {
		t.Fatalf("checkFeed: %v", err)
	}
	rel, err := update.VerifyFeed(feed, keys)
	if err != nil {
		t.Fatal(err)
	}
	if rel.Channel != "stable" || rel.SourceSHA != testSHA || rel.Date != testDate || len(rel.Artifacts) != len(targets) {
		t.Fatalf("unexpected release %+v", rel)
	}

	for _, target := range targets {
		goos, goarch, _ := strings.Cut(target, "-")
		a, ok := rel.ArtifactFor(goos, goarch)
		if !ok {
			t.Fatalf("no artifact for %s", target)
		}
		want := srv.URL + "/update/" + testVersion + "/" + artifactName(testVersion, target) + "?sha256=" + a.SHA256
		if a.URL != want {
			t.Fatalf("%s url = %s, want %s", target, a.URL, want)
		}
		got, err := update.Download(srv.Client(), a, t.TempDir())
		if err != nil {
			t.Fatalf("download %s: %v", target, err)
		}
		staged, err := update.Stage(got, filepath.Join(t.TempDir(), "htmlclay"))
		if err != nil {
			t.Fatalf("stage %s: %v", target, err)
		}
		b, err := os.ReadFile(staged)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(b, binaries[target]) {
			t.Fatalf("%s staged %q, want %q", target, b, binaries[target])
		}
	}
}

func TestPayloadRefusesAnIncompleteOrUnexpectedSet(t *testing.T) {
	t.Run("missing", func(t *testing.T) {
		dir := t.TempDir()
		writeArtifacts(t, dir)
		os.Remove(filepath.Join(dir, artifactName(testVersion, "windows-arm64")))
		if _, err := buildPayload(dir, testVersion, testSHA, defaultBaseURL, testDate); err == nil || !strings.Contains(err.Error(), "windows-arm64") {
			t.Fatalf("err = %v, want one naming windows-arm64", err)
		}
	})
	t.Run("extra", func(t *testing.T) {
		dir := t.TempDir()
		writeArtifacts(t, dir)
		os.WriteFile(filepath.Join(dir, "htmlclay-1.13.0-linux-amd64.gz"), []byte("old"), 0o644)
		if _, err := buildPayload(dir, testVersion, testSHA, defaultBaseURL, testDate); err == nil {
			t.Fatal("expected an error for a file from another version")
		}
	})
	t.Run("empty", func(t *testing.T) {
		dir := t.TempDir()
		writeArtifacts(t, dir)
		os.WriteFile(filepath.Join(dir, artifactName(testVersion, "linux-arm64")), nil, 0o644)
		if _, err := buildPayload(dir, testVersion, testSHA, defaultBaseURL, testDate); err == nil {
			t.Fatal("expected an error for an empty artifact")
		}
	})
}

func TestPayloadRefusesBadInputs(t *testing.T) {
	dir := t.TempDir()
	writeArtifacts(t, dir)
	cases := map[string][4]string{
		"version":  {"1.14", testSHA, defaultBaseURL, testDate},
		"sha":      {testVersion, "main", defaultBaseURL, testDate},
		"http url": {testVersion, testSHA, "http://download.htmlclay.com/update", testDate},
		"date":     {testVersion, testSHA, defaultBaseURL, "yesterday"},
	}
	for name, c := range cases {
		if _, err := buildPayload(dir, c[0], c[1], c[2], c[3]); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

func TestCheckFeedRefuses(t *testing.T) {
	dir := t.TempDir()
	writeArtifacts(t, dir)
	payload, err := buildPayload(dir, testVersion, testSHA, defaultBaseURL, testDate)
	if err != nil {
		t.Fatal(err)
	}
	feed, keys := signedFeed(t, payload)

	if err := checkFeed(feed, testVersion, map[string]ed25519.PublicKey{}); err == nil || !strings.Contains(err.Error(), "keys.go") {
		t.Errorf("no trusted keys: err = %v", err)
	}
	other, _ := signedFeed(t, payload)
	if err := checkFeed(other, testVersion, keys); err != update.ErrUntrusted {
		t.Errorf("untrusted signer: err = %v", err)
	}
	if err := checkFeed(feed, "1.14.1", keys); err == nil {
		t.Error("wrong version: expected an error")
	}
}

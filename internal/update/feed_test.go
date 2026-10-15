package update

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func testKeys(t *testing.T) (ed25519.PrivateKey, map[string]ed25519.PublicKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return priv, map[string]ed25519.PublicKey{"test-a": pub}
}

func payloadJSON(t *testing.T, rel Release) []byte {
	t.Helper()
	b, err := json.Marshal(rel)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func goodRelease() Release {
	return Release{
		Schema: 1, App: "htmlclay", Channel: "stable", Version: "1.14.0",
		Artifacts: map[string]Artifact{
			"linux-amd64": {URL: "https://download.htmlclay.com/update/1.14.0/htmlclay-1.14.0-linux-amd64.gz", SHA256: strings.Repeat("a", 64), Size: 10},
		},
	}
}

func TestVerifyFeedAcceptsSignedFeed(t *testing.T) {
	priv, keys := testKeys(t)
	feed, err := SignPayload(payloadJSON(t, goodRelease()), "test-a", priv)
	if err != nil {
		t.Fatal(err)
	}
	rel, err := VerifyFeed(feed, keys)
	if err != nil {
		t.Fatalf("VerifyFeed: %v", err)
	}
	if rel.Version != "1.14.0" || !rel.Newer("1.13.3") || rel.Newer("1.14.0") {
		t.Fatalf("unexpected release %+v", rel)
	}
	if _, ok := rel.ArtifactFor("linux", "amd64"); !ok {
		t.Fatal("expected a linux-amd64 artifact")
	}
	if _, ok := rel.ArtifactFor("windows", "arm64"); ok {
		t.Fatal("did not expect a windows-arm64 artifact")
	}
}

func TestVerifyFeedRejectsTamperedPayload(t *testing.T) {
	priv, keys := testKeys(t)
	feed, _ := SignPayload(payloadJSON(t, goodRelease()), "test-a", priv)
	var env Envelope
	json.Unmarshal(feed, &env)
	evil := goodRelease()
	evil.Artifacts["linux-amd64"] = Artifact{URL: "https://evil.example/x", SHA256: strings.Repeat("b", 64), Size: 10}
	env.Payload = base64.StdEncoding.EncodeToString(payloadJSON(t, evil))
	tampered, _ := json.Marshal(env)
	if _, err := VerifyFeed(tampered, keys); !errors.Is(err, ErrUntrusted) {
		t.Fatalf("want ErrUntrusted, got %v", err)
	}
}

func TestVerifyFeedRejectsUnknownKey(t *testing.T) {
	priv, _ := testKeys(t)
	_, otherKeys := testKeys(t)
	feed, _ := SignPayload(payloadJSON(t, goodRelease()), "test-a", priv)
	if _, err := VerifyFeed(feed, otherKeys); !errors.Is(err, ErrUntrusted) {
		t.Fatalf("want ErrUntrusted, got %v", err)
	}
}

func TestVerifyFeedRejectsBadFields(t *testing.T) {
	priv, keys := testKeys(t)
	cases := map[string]func(*Release){
		"wrong app":     func(r *Release) { r.App = "other" },
		"loose version": func(r *Release) { r.Version = "1.14" },
		"http url": func(r *Release) {
			r.Artifacts["linux-amd64"] = Artifact{URL: "http://download.htmlclay.com/x", SHA256: strings.Repeat("a", 64), Size: 10}
		},
		"no size": func(r *Release) {
			r.Artifacts["linux-amd64"] = Artifact{URL: "https://download.htmlclay.com/x", SHA256: strings.Repeat("a", 64)}
		},
		"short sha": func(r *Release) {
			r.Artifacts["linux-amd64"] = Artifact{URL: "https://download.htmlclay.com/x", SHA256: "abc", Size: 10}
		},
		"future schema": func(r *Release) { r.Schema = 2 },
	}
	for name, mutate := range cases {
		rel := goodRelease()
		mutate(&rel)
		feed, _ := SignPayload(payloadJSON(t, rel), "test-a", priv)
		if _, err := VerifyFeed(feed, keys); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

func serve(t *testing.T, body []byte) *httptest.Server {
	t.Helper()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func artifactFor(srv *httptest.Server, body []byte, size int64) Artifact {
	sum := sha256.Sum256(body)
	return Artifact{URL: srv.URL + "/update/1.14.0/htmlclay-test.bin", SHA256: hex.EncodeToString(sum[:]), Size: size}
}

func TestDownloadKeepsMatchingFile(t *testing.T) {
	body := []byte("new htmlclay binary")
	srv := serve(t, body)
	dir := t.TempDir()
	got, err := Download(srv.Client(), artifactFor(srv, body, int64(len(body))), dir)
	if err != nil {
		t.Fatalf("Download: %v", err)
	}
	if filepath.Base(got) != "htmlclay-test.bin" {
		t.Fatalf("unexpected path %s", got)
	}
	data, _ := os.ReadFile(got)
	if string(data) != string(body) {
		t.Fatal("downloaded bytes differ")
	}
}

func TestDownloadRejectsOversizeAndWrongHash(t *testing.T) {
	body := []byte("new htmlclay binary")
	srv := serve(t, body)
	dir := t.TempDir()

	short := artifactFor(srv, body, int64(len(body)-1))
	if _, err := Download(srv.Client(), short, dir); err == nil {
		t.Fatal("expected a size error when the server sends more than advertised")
	}

	wrong := artifactFor(srv, body, int64(len(body)))
	wrong.SHA256 = strings.Repeat("0", 64)
	if _, err := Download(srv.Client(), wrong, dir); err == nil {
		t.Fatal("expected a sha256 error")
	}

	entries, _ := os.ReadDir(dir)
	if len(entries) != 0 {
		t.Fatalf("a rejected download left files behind: %v", entries)
	}
}

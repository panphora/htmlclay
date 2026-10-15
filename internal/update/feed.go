package update

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
)

type Artifact struct {
	URL    string `json:"url"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

type Release struct {
	Schema    int                 `json:"schema"`
	App       string              `json:"app"`
	Channel   string              `json:"channel"`
	Version   string              `json:"version"`
	Date      string              `json:"date"`
	SourceSHA string              `json:"sourceSha"`
	Artifacts map[string]Artifact `json:"artifacts"`
}

type Signature struct {
	KeyID string `json:"keyId"`
	Sig   string `json:"sig"`
}

type Envelope struct {
	Payload    string      `json:"payload"`
	Signatures []Signature `json:"signatures"`
}

var ErrUntrusted = errors.New("update feed is not signed by a trusted key")

var strictVersion = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+$`)
var sha256Hex = regexp.MustCompile(`^[0-9a-f]{64}$`)

// VerifyFeed checks the signature before it reads anything else from the feed, so an
// unsigned or tampered feed never gets as far as naming a URL.
func VerifyFeed(data []byte, keys map[string]ed25519.PublicKey) (*Release, error) {
	var env Envelope
	if err := json.Unmarshal(data, &env); err != nil {
		return nil, fmt.Errorf("update feed: %w", err)
	}
	payload, err := base64.StdEncoding.DecodeString(env.Payload)
	if err != nil {
		return nil, fmt.Errorf("update feed payload: %w", err)
	}
	trusted := false
	for _, s := range env.Signatures {
		key, ok := keys[s.KeyID]
		if !ok {
			continue
		}
		sig, err := base64.StdEncoding.DecodeString(s.Sig)
		if err != nil || len(sig) != ed25519.SignatureSize {
			continue
		}
		if ed25519.Verify(key, payload, sig) {
			trusted = true
			break
		}
	}
	if !trusted {
		return nil, ErrUntrusted
	}

	var rel Release
	if err := json.Unmarshal(payload, &rel); err != nil {
		return nil, fmt.Errorf("update feed payload: %w", err)
	}
	if rel.Schema != 1 {
		return nil, fmt.Errorf("update feed schema %d is not supported", rel.Schema)
	}
	if rel.App != "htmlclay" {
		return nil, fmt.Errorf("update feed is for %q, not htmlclay", rel.App)
	}
	if !strictVersion.MatchString(rel.Version) {
		return nil, fmt.Errorf("update feed version %q is not X.Y.Z", rel.Version)
	}
	for target, a := range rel.Artifacts {
		u, err := url.Parse(a.URL)
		if err != nil || u.Scheme != "https" || u.Host == "" {
			return nil, fmt.Errorf("update feed artifact %s has a non-https url", target)
		}
		if !sha256Hex.MatchString(a.SHA256) || a.Size <= 0 {
			return nil, fmt.Errorf("update feed artifact %s has no valid sha256 and size", target)
		}
	}
	return &rel, nil
}

func SignPayload(payload []byte, keyID string, key ed25519.PrivateKey) ([]byte, error) {
	env := Envelope{
		Payload:    base64.StdEncoding.EncodeToString(payload),
		Signatures: []Signature{{KeyID: keyID, Sig: base64.StdEncoding.EncodeToString(ed25519.Sign(key, payload))}},
	}
	return json.MarshalIndent(env, "", "  ")
}

func (r *Release) Newer(current string) bool {
	return compareVersions(r.Version, current) > 0
}

func (r *Release) ArtifactFor(goos, goarch string) (Artifact, bool) {
	a, ok := r.Artifacts[goos+"-"+goarch]
	return a, ok
}

// Download writes the artifact into dir under its URL's file name, reading at most one
// byte past the advertised size, and keeps it only when size and sha256 both match.
func Download(client *http.Client, a Artifact, dir string) (string, error) {
	u, err := url.Parse(a.URL)
	if err != nil {
		return "", err
	}
	name := path.Base(u.Path)
	if name == "." || name == "/" || name == "" {
		return "", fmt.Errorf("artifact url %q has no file name", a.URL)
	}

	resp, err := client.Get(a.URL)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download %s: HTTP %d", name, resp.StatusCode)
	}

	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	tmp, err := os.CreateTemp(dir, "."+name+".*")
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp.Name())

	hash := sha256.New()
	n, err := io.Copy(io.MultiWriter(tmp, hash), io.LimitReader(resp.Body, a.Size+1))
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return "", err
	}
	if n != a.Size {
		return "", fmt.Errorf("download %s: got %d bytes, want %d", name, n, a.Size)
	}
	if got := hex.EncodeToString(hash.Sum(nil)); got != a.SHA256 {
		return "", fmt.Errorf("download %s: sha256 %s does not match the feed", name, got)
	}

	final := filepath.Join(dir, name)
	if err := os.Rename(tmp.Name(), final); err != nil {
		return "", err
	}
	return final, nil
}

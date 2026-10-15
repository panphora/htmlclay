// Command mkfeed builds and checks the HTML Clay update feed in the release workflow.
//
//	mkfeed payload -version V -source-sha SHA [-base-url URL] [-date RFC3339] DIR
//	    prints the schema 1 payload naming the update artifacts in DIR
//	mkfeed verify -version V feed.json
//	    checks a signed feed against the keys compiled into the app
package main

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/panphora/htmlclay/internal/update"
)

const defaultBaseURL = "https://download.htmlclay.com/update"

// targets are the platforms that update themselves from the feed. macOS is not
// here: it will update through Sparkle.
var targets = []string{"linux-amd64", "linux-arm64", "windows-amd64", "windows-arm64"}

var fullSHA = regexp.MustCompile(`^[0-9a-f]{40}$`)

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	switch os.Args[1] {
	case "payload":
		fs := flag.NewFlagSet("payload", flag.ExitOnError)
		version := fs.String("version", "", "release version, X.Y.Z")
		sourceSHA := fs.String("source-sha", "", "full commit id the release was built from")
		baseURL := fs.String("base-url", defaultBaseURL, "https url the update/ prefix is served from")
		date := fs.String("date", "", "release time, RFC 3339 (default now)")
		fs.Parse(os.Args[2:])
		if fs.NArg() != 1 {
			usage()
		}
		if *date == "" {
			*date = time.Now().UTC().Format(time.RFC3339)
		}
		out, err := buildPayload(fs.Arg(0), *version, *sourceSHA, *baseURL, *date)
		if err != nil {
			fail(err)
		}
		os.Stdout.Write(append(out, '\n'))
	case "verify":
		fs := flag.NewFlagSet("verify", flag.ExitOnError)
		version := fs.String("version", "", "release version the feed must name")
		fs.Parse(os.Args[2:])
		if fs.NArg() != 1 {
			usage()
		}
		data, err := os.ReadFile(fs.Arg(0))
		if err != nil {
			fail(err)
		}
		if err := checkFeed(data, *version, update.TrustedKeys); err != nil {
			fail(err)
		}
		fmt.Printf("feed for %s verifies against the compiled-in keys\n", *version)
	default:
		usage()
	}
}

func artifactName(version, target string) string {
	ext := ".gz"
	if strings.HasPrefix(target, "windows-") {
		ext = ".exe"
	}
	return "htmlclay-" + version + "-" + target + ext
}

// buildPayload hashes the exact files in dir, which must be one update artifact
// per target and nothing else. Each url carries the artifact's sha256 as a query,
// so a rebuild of the same version (release.sh --resume) gets a new cache key
// instead of a year-long cached copy of the bytes it replaced.
func buildPayload(dir, version, sourceSHA, baseURL, date string) ([]byte, error) {
	if !update.IsStrictVersion(version) {
		return nil, fmt.Errorf("version %q is not X.Y.Z", version)
	}
	if !fullSHA.MatchString(sourceSHA) {
		return nil, fmt.Errorf("source sha %q is not a full commit id", sourceSHA)
	}
	if !strings.HasPrefix(baseURL, "https://") {
		return nil, fmt.Errorf("base url %q is not https", baseURL)
	}
	if _, err := time.Parse(time.RFC3339, date); err != nil {
		return nil, fmt.Errorf("date %q is not RFC 3339", date)
	}
	baseURL = strings.TrimRight(baseURL, "/")

	want := map[string]string{}
	for _, t := range targets {
		want[artifactName(version, t)] = t
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		if _, ok := want[e.Name()]; !ok {
			return nil, fmt.Errorf("%s is not an update artifact for %s", e.Name(), version)
		}
	}

	rel := update.Release{
		Schema: 1, App: "htmlclay", Channel: "stable", Version: version,
		Date: date, SourceSHA: sourceSHA, Artifacts: map[string]update.Artifact{},
	}
	for _, t := range targets {
		name := artifactName(version, t)
		sum, size, err := hashFile(filepath.Join(dir, name))
		if err != nil {
			return nil, fmt.Errorf("update artifact for %s: %w", t, err)
		}
		if size == 0 {
			return nil, fmt.Errorf("update artifact %s is empty", name)
		}
		rel.Artifacts[t] = update.Artifact{
			URL:    baseURL + "/" + version + "/" + name + "?sha256=" + sum,
			SHA256: sum,
			Size:   size,
		}
	}
	return json.MarshalIndent(rel, "", "  ")
}

func hashFile(path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

// checkFeed is the app's own check plus what the release promises: the feed
// names this version, on the stable channel, with an artifact for every target.
func checkFeed(data []byte, version string, keys map[string]ed25519.PublicKey) error {
	if len(keys) == 0 {
		return errors.New("internal/update/keys.go trusts no key, so no installed app would accept this feed; add the public key there first")
	}
	rel, err := update.VerifyFeed(data, keys)
	if err != nil {
		return err
	}
	if rel.Version != version {
		return fmt.Errorf("feed names version %s, want %s", rel.Version, version)
	}
	if rel.Channel != "stable" {
		return fmt.Errorf("feed channel is %q, want stable", rel.Channel)
	}
	for _, t := range targets {
		if _, ok := rel.Artifacts[t]; !ok {
			return fmt.Errorf("feed has no artifact for %s", t)
		}
	}
	return nil
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: mkfeed payload -version V -source-sha SHA [-base-url URL] [-date RFC3339] DIR | mkfeed verify -version V feed.json")
	os.Exit(2)
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "mkfeed:", err)
	os.Exit(1)
}

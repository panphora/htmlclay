package update

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

// DefaultFeedURL is the signed stable feed. HTMLCLAY_UPDATE_FEED may point the
// app at another one, which is safe because a feed is only believed when it is
// signed by one of TrustedKeys.
const DefaultFeedURL = "https://download.htmlclay.com/update/stable.json"

const (
	// defaultFirstCheck keeps the first check out of the way of startup, when
	// the app is binding its port and restoring sites.
	defaultFirstCheck = 60 * time.Second
	// defaultCheckInterval is what makes "the update is already downloaded"
	// true for an app that runs from the tray for weeks.
	defaultCheckInterval = 6 * time.Hour
	feedTimeout          = 30 * time.Second
	maxFeedSize          = 1 << 20
	seenFileName         = "seen.json"
	seenSchema           = 1
)

// Ready is an update that is downloaded, verified and staged beside the
// install, so applying it is a rename and a restart.
type Ready struct {
	Version    string
	StagedPath string
}

// Updater checks the signed feed and stages a newer release for this
// platform. It never applies anything: the person does, from the tray.
type Updater struct {
	FeedURL     string
	Keys        map[string]ed25519.PublicKey
	Client      *http.Client
	Current     string
	GOOS        string
	GOARCH      string
	Dir         string
	InstallPath string
	// FirstCheck and Interval default to 60 s and 6 h; tests shorten them.
	FirstCheck time.Duration
	Interval   time.Duration
	Logf       func(format string, args ...any)
}

// seen is updates/seen.json, the highest version any verified feed has
// offered this install. A feed offering less is a replay of an old signed feed
// and is refused. Version N+1 reads what version N wrote, so like state.json
// it only ever gains fields.
type seen struct {
	Schema  int    `json:"schema"`
	Highest string `json:"highest"`
}

// IsStrictVersion reports whether v is X.Y.Z, the only form the updater
// compares. A dev build's version is anything else, and never updates itself.
func IsStrictVersion(v string) bool {
	return strictVersion.MatchString(v)
}

// Run checks after FirstCheck and then every Interval until an update is
// staged, hands it to ready once, and returns. A failed check is logged and
// retried at the next interval; it is never an error the person sees. Run
// also returns when stop is closed; a nil stop never closes.
func (u *Updater) Run(stop <-chan struct{}, ready func(Ready)) {
	wait := u.FirstCheck
	if wait <= 0 {
		wait = defaultFirstCheck
	}
	interval := u.Interval
	if interval <= 0 {
		interval = defaultCheckInterval
	}
	for {
		select {
		case <-stop:
			return
		case <-time.After(wait):
		}
		r, err := u.CheckOnce()
		if err != nil {
			u.logf("Update check failed, retrying in %s: %v", interval, err)
		} else if r != nil {
			ready(*r)
			return
		}
		wait = interval
	}
}

// CheckOnce fetches and verifies the feed and, when it offers a newer release
// for this platform, downloads and stages it. It returns nil, nil when there
// is nothing to do.
func (u *Updater) CheckOnce() (*Ready, error) {
	if err := os.MkdirAll(u.Dir, 0o700); err != nil {
		return nil, err
	}
	rel, err := u.fetchFeed()
	if err != nil {
		return nil, err
	}
	highest, err := u.readSeen()
	if err != nil {
		u.logf("Ignoring an unreadable %s: %v", seenFileName, err)
	}
	if highest != "" && compareVersions(rel.Version, highest) < 0 {
		return nil, fmt.Errorf("the feed offers %s, older than %s already seen", rel.Version, highest)
	}
	if compareVersions(rel.Version, highest) > 0 {
		if err := writeJSONAtomic(filepath.Join(u.Dir, seenFileName), &seen{Schema: seenSchema, Highest: rel.Version}); err != nil {
			u.logf("Could not record the highest version seen: %v", err)
		}
	}
	if !rel.Newer(u.Current) {
		return nil, nil
	}
	// A version the supervisor rolled back stays skipped until a newer one
	// appears; offering it again would only fail the same way.
	if st, _ := ReadState(u.Dir); st != nil && st.Failed != nil && st.Failed.Version == rel.Version {
		u.logf("Skipping update %s: it failed before (%s)", rel.Version, st.Failed.Reason)
		return nil, nil
	}
	a, ok := rel.ArtifactFor(u.GOOS, u.GOARCH)
	if !ok {
		return nil, fmt.Errorf("release %s has no artifact for %s-%s", rel.Version, u.GOOS, u.GOARCH)
	}
	verDir := filepath.Join(u.Dir, rel.Version)
	// The download is only an intermediate: Stage leaves the binary beside the
	// install, so this folder never outlives the check.
	defer os.RemoveAll(verDir)
	path, err := Download(u.Client, a, verDir)
	if err != nil {
		return nil, err
	}
	staged, err := Stage(path, u.InstallPath)
	if err != nil {
		return nil, err
	}
	u.logf("Update %s is downloaded and staged at %s", rel.Version, staged)
	return &Ready{Version: rel.Version, StagedPath: staged}, nil
}

func (u *Updater) fetchFeed() (*Release, error) {
	ctx, cancel := context.WithTimeout(context.Background(), feedTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.FeedURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := u.Client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("update feed: HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxFeedSize+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxFeedSize {
		return nil, errors.New("update feed is larger than 1 MiB")
	}
	return VerifyFeed(data, u.Keys)
}

func (u *Updater) readSeen() (string, error) {
	var s seen
	ok, err := readJSON(filepath.Join(u.Dir, seenFileName), &s)
	if err != nil || !ok {
		return "", err
	}
	if !IsStrictVersion(s.Highest) {
		return "", fmt.Errorf("highest version %q is not X.Y.Z", s.Highest)
	}
	return s.Highest, nil
}

func (u *Updater) logf(format string, args ...any) {
	if u.Logf != nil {
		u.Logf(format, args...)
	}
}

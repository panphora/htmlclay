package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/panphora/htmlclay/internal/session"
)

// updateHandoffFile is the name of the update handoff in the config directory.
const updateHandoffFile = "update-handoff.json"

// updateHandoffSchema is frozen from the first updater-capable release: version N
// writes the handoff that version N+1 reads, so a reader must keep accepting every
// schema a supported writer produced.
const updateHandoffSchema = 1

// updateHandoffMaxAge bounds how long a handoff stays usable. An update restart
// takes seconds; anything older is a leftover from an update that never finished,
// and replaying it on some later ordinary launch would resurrect capabilities the
// person had every reason to think were gone.
const updateHandoffMaxAge = 60 * time.Second

// updateHandoff is what an update restart carries from the old process to the
// new one, so every tab that was open keeps saving at the address it already has.
// It holds save tokens, so it is written 0600 and deleted on first read.
type updateHandoff struct {
	Schema        int                 `json:"schema"`
	Transaction   string              `json:"transaction"`
	Created       time.Time           `json:"created"`
	TargetVersion string              `json:"targetVersion"`
	Port          int                 `json:"port"`
	Sites         []updateHandoffSite `json:"sites"`
}

type updateHandoffSite struct {
	Key          string               `json:"key"`
	Slug         string               `json:"slug"`
	Anchor       string               `json:"anchor"`
	Trusted      bool                 `json:"trusted"`
	Files        []updateHandoffEntry `json:"files"`
	GrantedRoots []string             `json:"grantedRoots,omitempty"`
}

type updateHandoffEntry struct {
	Path  string             `json:"path"`
	Via   session.Provenance `json:"via"`
	Token string             `json:"token"`
}

// writeUpdateHandoff captures every live site's registrations and grants into
// path, for the process that targetVersion will start. It preserves only
// capabilities that already exist; it never widens one. Nothing calls it yet
// except tests: the updater will, after it has drained in-flight work and before
// it stops this process.
func (a *app) writeUpdateHandoff(path, targetVersion string) error {
	txn := make([]byte, 16)
	if _, err := rand.Read(txn); err != nil {
		return fmt.Errorf("cannot generate a transaction id: %w", err)
	}
	h := updateHandoff{
		Schema:        updateHandoffSchema,
		Transaction:   hex.EncodeToString(txn),
		Created:       time.Now().UTC(),
		TargetVersion: targetVersion,
		Port:          a.rt.port,
	}

	a.mu.Lock()
	sites := append([]*site(nil), a.sites...)
	a.mu.Unlock()
	for _, s := range sites {
		if s.closed.Load() {
			continue
		}
		snap := s.sessions.Snapshot()
		if len(snap.Registrations) == 0 && len(snap.GrantedRoots) == 0 {
			continue
		}
		entry := updateHandoffSite{
			Key:          s.key,
			Slug:         s.slug,
			Anchor:       s.anchor,
			Trusted:      s.trusted,
			GrantedRoots: snap.GrantedRoots,
		}
		for _, r := range snap.Registrations {
			entry.Files = append(entry.Files, updateHandoffEntry{Path: r.Path, Via: r.Via, Token: r.Token})
		}
		h.Sites = append(h.Sites, entry)
	}

	data, err := json.MarshalIndent(h, "", "  ")
	if err != nil {
		return err
	}
	return writePrivateFile(path, data)
}

// writePrivateFile writes data to path atomically with owner-only permissions:
// a temp file in the same directory, chmod 0600 before any token is visible
// under the final name, then rename.
func writePrivateFile(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".update-handoff-*.json")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(0600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpPath, path)
}

// restoreUpdateHandoff reads the handoff at path, deletes it, and rebuilds the
// sites and registrations it describes with their original slugs and tokens. It
// runs at startup after startSites, so a trusted folder's site already exists
// and only gets its registrations back, and before argv is processed.
//
// A missing file is the ordinary launch and returns (0, nil). The file is
// deleted before it is parsed, so a handoff can be consumed at most once
// whatever happens next. A stale handoff, one for another version, or one for
// another port is ignored whole. Inside a usable handoff each site and each file
// is checked on its own, and anything that no longer validates is skipped and
// logged; restoring is never fatal. It returns how many registrations came back.
func (a *app) restoreUpdateHandoff(path string) (int, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if rmErr := os.Remove(path); rmErr != nil && !errors.Is(rmErr, os.ErrNotExist) {
		a.rt.logger.Printf("Could not delete the update handoff %s: %v", path, rmErr)
	}
	if err != nil {
		return 0, fmt.Errorf("cannot read the update handoff: %w", err)
	}

	var h updateHandoff
	if err := json.Unmarshal(data, &h); err != nil {
		return 0, fmt.Errorf("cannot parse the update handoff: %w", err)
	}
	if h.Schema != updateHandoffSchema {
		return 0, fmt.Errorf("update handoff schema %d is not supported", h.Schema)
	}
	if age := time.Since(h.Created); age < 0 || age > updateHandoffMaxAge {
		return 0, fmt.Errorf("update handoff %s is stale (created %s)", h.Transaction, h.Created.Format(time.RFC3339))
	}
	if h.TargetVersion != version {
		return 0, fmt.Errorf("update handoff %s targets version %s, this is %s", h.Transaction, h.TargetVersion, version)
	}
	// Every origin is slug.localhost:port. On another port none of the old
	// addresses exist, so restoring tokens would only re-arm capabilities no open
	// tab can reach.
	if h.Port != a.rt.port {
		return 0, fmt.Errorf("update handoff %s is for port %d, this process serves %d", h.Transaction, h.Port, a.rt.port)
	}

	restored := 0
	for _, entry := range h.Sites {
		restored += a.restoreHandoffSite(entry)
	}
	a.rt.logger.Printf("Update handoff %s restored %d registration(s)", h.Transaction, restored)
	return restored, nil
}

// restoreHandoffSite brings one site back and returns how many of its
// registrations were restored. A trusted site must already be live, because
// startSites only builds a trusted folder that is still declared and still
// matches its identity pin; when it is not live the trust is gone and so is
// everything it granted. An ad-hoc site is rebuilt through buildSite, the same
// path route uses, and published only if at least one file came back.
func (a *app) restoreHandoffSite(entry updateHandoffSite) int {
	// The slug must still be the one config remembers for this key. That is what
	// makes the restored host the host the tab is on, and it refuses an entry
	// whose mapping changed (a retired or reassigned slug) while the app was down.
	if a.rt.cfg.SiteSlug(entry.Key) != entry.Slug {
		a.rt.logger.Printf("Update handoff: %s is no longer remembered under %s; skipping it", entry.Key, entry.Slug)
		return 0
	}

	if entry.Trusted {
		a.hostMu.RLock()
		s := a.bySlug[entry.Slug]
		a.hostMu.RUnlock()
		if s == nil || s.closed.Load() || !s.trusted || s.key != entry.Key {
			a.rt.logger.Printf("Update handoff: trusted folder %s has no live site; skipping it", entry.Anchor)
			return 0
		}
		return a.restoreHandoffFiles(s, entry)
	}

	a.hostMu.RLock()
	_, taken := a.bySlug[entry.Slug]
	a.hostMu.RUnlock()
	if taken {
		a.rt.logger.Printf("Update handoff: %s is already live; skipping it", entry.Slug)
		return 0
	}
	// A folder trusted since the old process registered these files belongs to
	// that folder's origin now, and an ad-hoc site for it would break invariant 4.
	if _, ok := a.trustedAnchor(entry.Anchor); ok {
		a.rt.logger.Printf("Update handoff: %s is inside a trusted folder now; skipping it", entry.Anchor)
		return 0
	}

	s := a.buildSite(entry.Anchor, entry.Key, false)
	n := a.restoreHandoffFiles(s, entry)

	a.mu.Lock()
	_, taken = a.bySlug[entry.Slug]
	if n == 0 || a.stopping || taken || s.slug != entry.Slug {
		a.discardPendingLocked(s)
		a.mu.Unlock()
		s.close()
		return 0
	}
	a.publishLocked(s)
	a.mu.Unlock()
	a.rt.logger.Printf("Update handoff: site restored for %s at http://%s", entry.Anchor, s.host)
	return n
}

// restoreHandoffFiles re-registers each file under its old token and then
// reinstalls the site's grants. Each file is validated on its own: it must sit
// under the site's anchor, must not have moved under a trusted folder's origin,
// and must pass RestoreRegistration, which re-runs every Register check.
func (a *app) restoreHandoffFiles(s *site, entry updateHandoffSite) int {
	n := 0
	for _, f := range entry.Files {
		if !session.EqualOrUnder(f.Path, s.anchor) {
			a.rt.logger.Printf("Update handoff: %s is outside %s; skipping it", f.Path, s.anchor)
			continue
		}
		if !entry.Trusted {
			if _, ok := a.trustedAnchor(f.Path); ok {
				a.rt.logger.Printf("Update handoff: %s is inside a trusted folder now; skipping it", f.Path)
				continue
			}
		}
		if _, err := s.sessions.RestoreRegistration(f.Path, f.Via, f.Token); err != nil {
			a.rt.logger.Printf("Update handoff: could not restore %s: %v", f.Path, err)
			continue
		}
		n++
	}
	if n == 0 {
		return 0
	}
	for _, root := range entry.GrantedRoots {
		// GrantCanonicalRoot deliberately does not re-resolve, so the check that
		// the folder is still the one the person approved happens here: a grant
		// whose path now resolves somewhere else is dropped, never followed.
		if resolved, err := resolveSymlinks(root); err != nil || resolved != root {
			a.rt.logger.Printf("Update handoff: the grant on %s no longer resolves to itself; skipping it", root)
			continue
		}
		if err := s.sessions.GrantCanonicalRoot(root); err != nil {
			a.rt.logger.Printf("Update handoff: could not restore the grant on %s: %v", root, err)
		}
	}
	return n
}

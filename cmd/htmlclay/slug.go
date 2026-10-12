package main

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
)

// slugBaseMax bounds the readable part of a label. With the 5-character suffix the label
// stays well under the 63-character hostname label limit.
const slugBaseMax = 24

// slugBase turns a folder or file name into the readable part of its site label:
// lowercase, runs of anything outside [a-z0-9] collapsed to one hyphen, no hyphen at
// either end, cut at slugBaseMax, and "site" when nothing survives.
func slugBase(name string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(name) {
		if b.Len() >= slugBaseMax {
			break
		}
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			dash = false
			continue
		}
		if b.Len() > 0 && !dash {
			b.WriteByte('-')
			dash = true
		}
	}
	base := strings.Trim(b.String(), "-")
	if base == "" {
		return "site"
	}
	return base
}

// siteHost is the Host header value for a slug on the app's port.
func siteHost(slug string, port int) string {
	return fmt.Sprintf("%s.localhost:%d", slug, port)
}

// slugForLocked returns the label key is served under, allocating one the first time.
// name is what the readable part is made from: the folder's base name, or the file's stem
// for a file loose in home. The allocation is recorded in the config in memory only; the
// caller saves when it publishes the site. Caller holds a.mu, so two allocations cannot
// interleave between the uniqueness check and the record. A retired slug is never reused.
func (a *app) slugForLocked(key, name string) string {
	if slug := a.rt.cfg.SiteSlug(key); slug != "" {
		return slug
	}
	base := slugBase(name)
	for attempt := 0; ; attempt++ {
		n := 2
		if attempt >= 32 {
			n = 4
		}
		raw := make([]byte, n)
		rand.Read(raw)
		slug := base + "-" + hex.EncodeToString(raw)
		if a.rt.cfg.SlugInUse(slug) {
			continue
		}
		a.rt.cfg.RememberSiteSlug(key, slug)
		return slug
	}
}

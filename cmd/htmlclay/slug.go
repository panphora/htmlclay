package main

import "fmt"

// siteHost is the Host header value for a slug on the app's port.
func siteHost(slug string, port int) string {
	return fmt.Sprintf("%s.localhost:%d", slug, port)
}

// slugForLocked returns the label key is served under, allocating the next numbered one
// the first time. The allocation is recorded in the config in memory only; the caller
// saves when it publishes the site, and drops it through discardPendingLocked when it
// does not. Caller holds a.mu, so two allocations cannot interleave.
func (a *app) slugForLocked(key string) string {
	if slug := a.rt.cfg.SiteSlug(key); slug != "" {
		return slug
	}
	return a.rt.cfg.AllocateSiteSlug(key)
}

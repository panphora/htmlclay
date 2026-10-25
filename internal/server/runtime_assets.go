package server

import (
	"net/http"
	"path/filepath"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/panphora/htmlclay/internal/clayruntime"
	"github.com/panphora/htmlclay/internal/session"
)

// handleRuntimeAsset answers a ClayJS runtime file from the release embedded in
// this binary, so a document opened offline still loads its client. The URL is
// resolved rather than mapped: only a path the release index lists is ever read,
// so no filesystem path and no request segment reaches a file open.
func (s *Server) handleRuntimeAsset(w http.ResponseWriter, r *http.Request) {
	raw := "http://" + r.Host + r.URL.EscapedPath()
	if r.URL.RawQuery != "" {
		raw += "?" + r.URL.RawQuery
	}
	parsed := clayruntime.ParseRuntimeURL(raw, clayruntime.Options{
		Releases:        clayruntime.Releases,
		SelectedVersion: clayruntime.Selected,
		DocumentURL:     "http://" + r.Host + "/",
	})
	if parsed == nil || parsed.Kind != "local" {
		writeRuntimeAssetMiss(w)
		return
	}
	file, data, ok := clayruntime.Asset(parsed.Version, parsed.Path)
	if !ok {
		writeRuntimeAssetMiss(w)
		return
	}

	etag := `"` + file.SHA256 + `"`
	h := w.Header()
	h.Set("Content-Type", file.Type)
	h.Set("ETag", etag)
	h.Set("Cache-Control", "public, max-age=31536000, immutable")
	h.Set("X-Content-Type-Options", "nosniff")
	if strings.Contains(r.Header.Get("If-None-Match"), etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Write(data)
}

// writeRuntimeAssetMiss is the fixed answer for everything the release does not
// list, so a miss says nothing about what is or is not bundled.
func writeRuntimeAssetMiss(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusNotFound)
	w.Write([]byte("Not Found"))
}

var loopbackRuntime = regexp.MustCompile(`(?i)\bhttp://((?:localhost|127\.0\.0\.1)(?::\d{1,5})?)/_/clayjs/`)

// canonicalForWrite turns any ClayJS runtime URL this host projected back into its public URL and
// drops the projection descriptor, so the bytes that become the document are host-independent.
// Bytes with neither, and bytes that are not UTF-8, come back untouched. A write with no
// request Host, such as an export, reads the document URL off the first loopback origin the
// text names before /_/clayjs/, any port.
func canonicalForWrite(host string, data []byte) []byte {
	if !utf8.Valid(data) {
		return data
	}
	if host == "" {
		host = "localhost"
		if m := loopbackRuntime.FindSubmatch(data); m != nil {
			host = strings.ToLower(string(m[1]))
		}
	}
	result := clayruntime.CanonicalizeRuntimeHTML(data, clayruntime.Releases, "http://"+host+"/")
	if result.Changed == 0 && !result.DescriptorRemoved {
		return data
	}
	return []byte(result.HTML)
}

// projectRuntimeHTML rewrites a document's ClayJS runtime URLs to this host's bundled release,
// naming the document by documentURL. Bytes that are not UTF-8, and documents with no runtime
// URL to rewrite, come back untouched. Frames carry no descriptor: the receiving tab already
// holds one, and an inserted element would shift live sync's identity map.
func projectRuntimeHTML(data []byte, documentURL string) []byte {
	if !utf8.Valid(data) {
		return data
	}
	projection := clayruntime.ProjectRuntimeHTML(data, clayruntime.Options{
		Releases:        clayruntime.Releases,
		SelectedVersion: clayruntime.Selected,
		DocumentURL:     documentURL,
		NoDescriptor:    true,
	})
	if len(projection.Mappings) == 0 {
		return data
	}
	return []byte(projection.HTML)
}

// documentURLFor is the address a browser loaded f from: this server's own Host spelling and the
// file's path under the served home directory.
func (s *Server) documentURLFor(f *session.File) string {
	return "http://" + s.Host() + servedPathOf(f)
}

// servedPathOf is a file's path on the server that registered it.
func servedPathOf(f *session.File) string {
	return "/" + filepath.ToSlash(f.RelPath)
}

// documentURLWithoutHost is a file's served address when no one Host names it, as for the live
// watcher, which every site in the process shares. Projection writes path-only local URLs, so an
// originless document URL still projects a frame exactly as a served document is projected.
func documentURLWithoutHost(f *session.File) string {
	return "http://" + servedPathOf(f)
}

// projectForServe rewrites the ClayJS runtime URLs in a served document to this host's bundled
// release. A source read (Clay-Source: 1) and a file that is not UTF-8 get their bytes untouched.
// A document that cannot be projected is served as it is, with a header naming why, so a page
// loading a runtime URL this host cannot answer says so instead of failing with no sign.
func (s *Server) projectForServe(w http.ResponseWriter, r *http.Request, data []byte) []byte {
	if r.Header.Get("Clay-Source") == "1" {
		return data
	}
	if !utf8.Valid(data) {
		return data
	}
	projection := clayruntime.ProjectRuntimeHTML(data, clayruntime.Options{
		Releases:        clayruntime.Releases,
		SelectedVersion: clayruntime.Selected,
		DocumentURL:     "http://" + r.Host + r.URL.RequestURI(),
	})
	if unavailable := projection.Unavailable; unavailable != nil {
		detail := unavailable.Reason + " " + strings.Join(unavailable.Versions, ",")
		w.Header().Set("Clay-Runtime-Unavailable", detail)
		s.reportRuntimeUnavailable(r.URL.Path, detail)
	}
	if len(projection.Mappings) == 0 {
		return data
	}
	return []byte(projection.HTML)
}

// reportRuntimeUnavailable logs a document served with public runtime URLs once per path and
// detail, so a page loaded repeatedly does not fill the log with the same explanation.
func (s *Server) reportRuntimeUnavailable(path, detail string) {
	key := path + " " + detail
	s.runtimeNoticeMu.Lock()
	if s.runtimeNotices[key] {
		s.runtimeNoticeMu.Unlock()
		return
	}
	if s.runtimeNotices == nil {
		s.runtimeNotices = make(map[string]bool)
	}
	s.runtimeNotices[key] = true
	s.runtimeNoticeMu.Unlock()
	s.logger.Printf("Serving %s with public runtime URLs: %s", path, detail)
}

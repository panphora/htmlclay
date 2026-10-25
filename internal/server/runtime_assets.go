package server

import (
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/panphora/htmlclay/internal/clayruntime"
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

// projectForServe rewrites the ClayJS runtime URLs in a served document to this host's bundled
// release. A source read (Clay-Source: 1) and a file that is not UTF-8 get their bytes untouched.
func projectForServe(r *http.Request, data []byte) []byte {
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
	if len(projection.Mappings) == 0 {
		return data
	}
	return []byte(projection.HTML)
}

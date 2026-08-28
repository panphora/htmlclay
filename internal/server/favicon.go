package server

import (
	"bytes"
	_ "embed"
	"net/http"
	"time"
)

// The app's own icon, answered on the origin's two root favicon paths.
//
// Every document is served from a path under the user's home, so a browser's
// default request for /favicon.ico addresses $HOME/favicon.ico. That path can
// never be read: the home directory is refused as a read root and cannot be
// granted as one, so the request is denied without even prompting the user. The
// only symptom is that every document window shows a blank icon.
//
// The answer is a constant. Nothing here touches the filesystem, so it cannot
// report whether anything exists at that path and adds no way to enumerate the
// home tree. It also displaces nothing a page can currently reach: a folder's
// own icon is requested at that folder's own path, /Documents/notes/favicon.svg,
// and is served as an ordinary in-scope asset that never reaches this function.

//go:embed favicon.ico
var faviconICO []byte

//go:embed favicon.svg
var faviconSVG []byte

// A zero modtime tells ServeContent to omit Last-Modified rather than invent one.
// Freshness comes from Cache-Control, and the bytes change only when the binary does.
var faviconModTime = time.Time{}

// serveAppFavicon answers the request and reports true when the path is one of
// the origin's root favicon paths. Any other path is left alone.
func serveAppFavicon(w http.ResponseWriter, r *http.Request, path string) bool {
	var body []byte
	var ctype string
	switch path {
	case "favicon.ico":
		body, ctype = faviconICO, "image/x-icon"
	case "favicon.svg":
		body, ctype = faviconSVG, "image/svg+xml"
	default:
		return false
	}

	w.Header().Set("Content-Type", ctype)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "public, max-age=86400")
	http.ServeContent(w, r, path, faviconModTime, bytes.NewReader(body))
	return true
}

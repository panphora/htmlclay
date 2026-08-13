package server

import "net/http"

// sameOriginHeaders stamps req with the attestation a browser sends for a
// page's own request to its origin, which every gated /_/ route requires.
// Works for both server-side test requests (req.Host set) and client-side ones
// (host lives in req.URL).
func sameOriginHeaders(req *http.Request) *http.Request {
	host := req.Host
	if host == "" {
		host = req.URL.Host
	}
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	req.Header.Set("Origin", "http://"+host)
	return req
}

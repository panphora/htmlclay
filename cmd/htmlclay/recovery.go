package main

import (
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/panphora/htmlclay/internal/server"
)

// parked is a remembered port bound with no capability at all: no session
// manager, no read roots, no versions store, no broker, and no file-serving
// code. It exists so a bookmarked URL answers with a page instead of
// ERR_CONNECTION_REFUSED, which is what a refresh got before ports were bound
// at startup.
//
// It is a separate type from site precisely so that it is structurally
// incapable of serving a file, whatever a later change to the serve path does.
// A site with an empty session manager would still run the whole serve path: it
// would park out-of-scope requests and raise a native permission dialog for a
// bookmark that happened to land on a stale port, naming a folder the user was
// not looking at.
//
// Every path gets the same bytes. The page never echoes the requested path and
// never touches the disk, so it can never be asked which of your files exist.
type parked struct {
	anchor string // for the log and nothing else, never for the response body
	port   int
	ln     net.Listener
	srv    *http.Server
}

func (p *parked) close() {
	p.srv.Close()
	p.ln.Close()
}

// parkPort binds port and answers it with the recovery page. A port that is
// already taken is simply skipped: something else owns it now, and the origin
// will move to a fresh port the next time a file there is opened.
func (a *app) parkPort(anchor string, port int) {
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		a.rt.logger.Printf("Remembered port %d for %s is taken; not holding it", port, anchor)
		return
	}
	p := &parked{anchor: anchor, port: port, ln: ln}
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusNotFound)
		w.Write(recoveryPage)
	})
	p.srv = &http.Server{
		Handler:           server.HostValidationMiddleware(handler, port),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	a.mu.Lock()
	if a.stopping {
		a.mu.Unlock()
		p.close()
		return
	}
	a.parked = append(a.parked, p)
	a.mu.Unlock()
	go func() {
		if err := p.srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			a.rt.logger.Printf("Recovery listener error on %s: %v", anchor, err)
		}
	}()
	a.rt.logger.Printf("Holding remembered port %d for %s (recovery page)", port, anchor)
}

// unpark releases the recovery listener holding anchor's port, so a real site
// can bind that exact port and the bookmark keeps working. Without this, route
// would find the port taken by HTML Clay's own placeholder and move the origin,
// which is the one thing binding at startup exists to prevent.
func (a *app) unpark(anchor string) {
	a.mu.Lock()
	var found *parked
	kept := a.parked[:0]
	for _, p := range a.parked {
		if p.anchor == anchor && found == nil {
			found = p
			continue
		}
		kept = append(kept, p)
	}
	a.parked = kept
	a.mu.Unlock()
	if found != nil {
		found.close()
		a.rt.logger.Printf("Released remembered port %d for %s", found.port, anchor)
	}
}

var recoveryPage = []byte(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>HTML Clay</title>
<style>
  :root { color-scheme: light dark; }
  body { margin:0; min-height:100vh; display:flex; align-items:center; justify-content:center;
         font:15px/1.55 -apple-system,system-ui,Segoe UI,sans-serif; background:#f6f6f7; color:#1c1c1e; }
  @media (prefers-color-scheme: dark) { body { background:#141416; color:#f2f2f7; } }
  main { max-width:29rem; padding:2rem; }
  h1 { font-size:1.15rem; margin:0 0 .75rem; }
  p { margin:0 0 .75rem; }
  ul { margin:0; padding-left:1.15rem; }
  li { margin-bottom:.4rem; }
</style>
</head>
<body>
<main>
  <h1>Nothing is open at this address</h1>
  <p>HTML Clay is running and holding this address for you, but no file is being served here right now.</p>
  <ul>
    <li>Open the file again from Finder or your file manager, and this address will work.</li>
    <li>To keep it working for good, add the file's folder under <strong>Trusted Folders</strong>
        in the HTML Clay menu. Files in a trusted folder always open at the same address.</li>
  </ul>
</main>
</body>
</html>
`)

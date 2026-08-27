package server

import "github.com/panphora/htmlclay/internal/platform"

// Hooks are the decisions a site's server cannot make for itself. Each one
// needs the declared trusted-folder list, the app's site registry, or a native
// dialog, and the server imports none of those.
//
// Wiring them as one struct at construction makes the whole app-to-server
// contract a single declaration that can be read from either end. As five
// separate setters mutating a live server, it could only be reconstructed by
// grepping, and two of them had to take the broker's lock because they could be
// called at any moment.
//
// A nil field disables that route, which is what a standalone server (every
// server test) wants.
type Hooks struct {
	// Confirm replaces the native read-permission dialog. Nil uses the real one.
	//
	// allowTrust reports whether the dialog may offer to trust the folder
	// durably. The app decides it from its refusal rules BEFORE the dialog is
	// drawn, so a folder that could never be trusted is never offered; the
	// button used to be drawn always and the refusal arrived afterwards as a
	// notification, which asked the user for a choice that could not be honored.
	Confirm func(title, message string, allowTrust bool) (platform.ConfirmChoice, error)

	// TrustedCovers reports whether absPath sits inside a DECLARED trusted
	// folder. It is the config fact, not this site's runtime roots: a file can
	// belong to a trusted folder this site holds no root for, and answering from
	// the roots is exactly what made a folder declared below an already-open
	// site grant nothing at all. It must not touch the filesystem; see
	// shouldAutoRegister for why the ordering there is load-bearing.
	TrustedCovers func(absPath string) bool

	// TrustedLive is the same question with the identity pin applied: is absPath
	// inside a trusted folder that is still the folder that was trusted? A folder
	// deleted and recreated still covers lexically but is no longer live, and the
	// two must not be confused. TrustedCovers is the gate on whether to TRY
	// auto-registering, and answers from memory; this one is the standing
	// capability, and stats. Only handleTrustRequest asks it, on the file its own
	// token was minted for, so the stat reveals nothing a caller did not know.
	TrustedLive func(absPath string) bool

	// Route registers absPath through the app and reports where it serves, so
	// this site can redirect when the registration landed on another origin.
	// Trusted-folder auto-registration is its only caller, and the app refuses
	// anything that does not anchor at a live trusted folder.
	Route func(absPath string) (url string, ok bool)

	// MayTrustFolder gates Confirm's allowTrust for a candidate folder.
	MayTrustFolder func(dir string) bool

	// TrustFolder is the durable half of the read prompt's "Trust this folder"
	// choice. The folder there is one the PAGE steered, by choosing which
	// out-of-scope assets to request, so the app applies its stricter refusal
	// rule behind this.
	TrustFolder func(dir string) error

	// TrustRequest is the one door for "trust this file's folder". Both routes
	// into it name a FILE the server already vouches for, never a folder: the
	// banner's nonce resolves to the descriptor that was served, and the JSON
	// route's token to a registration. openedByUser is false when the file was
	// reached by a link, and the dialog says so on its first line.
	TrustRequest func(requestingFile string, openedByUser bool) (url string, ok bool)
}

// SetHooks wires the app behind this server. Call once, before Start.
func (s *Server) SetHooks(h Hooks) {
	s.hooks = h
	if h.Confirm != nil {
		s.broker.confirm = h.Confirm
	}
	s.broker.mayTrust = h.MayTrustFolder
	s.broker.trust = h.TrustFolder
}

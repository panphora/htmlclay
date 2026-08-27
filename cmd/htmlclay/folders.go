package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/panphora/htmlclay/internal/platform"
	"github.com/panphora/htmlclay/internal/session"
	"github.com/panphora/htmlclay/internal/tray"
	"github.com/panphora/htmlclay/internal/trust"
)

// trustFolder records dir as a trusted folder and brings it live. It returns a
// human-readable error for the caller to surface, nil on success.
func (a *app) trustFolder(dir string) error {
	canonical, err := a.rt.policy.Canonical(dir)
	if err != nil {
		return err
	}
	identity := platform.DirIdentity(canonical)

	a.mu.Lock()
	added := a.rt.cfg.AddTrustedFolder(canonical, identity)
	var previous string
	if !added {
		// Already listed. Approving for a folder whose stored identity no longer
		// matches — deleted and recreated by a restore, a sync client, or a clean
		// checkout — is a re-authorization of the folder now on disk, so re-pin it
		// and fall through to the save and the adopt below. Returning here instead
		// leaves the entry dead and the dialog asking on every page load, with
		// approving it never helping.
		previous, _ = a.rt.cfg.SetTrustedIdentity(canonical, identity)
	}
	if sErr := a.rt.cfg.Save(); sErr != nil {
		// Put the list back exactly as it was: drop an entry this call added,
		// restore the old pin on one it only re-pinned. Removing a pre-existing
		// entry here would revoke a folder the user still holds.
		if added {
			a.rt.cfg.RemoveTrustedFolder(canonical)
		} else {
			a.rt.cfg.SetTrustedIdentity(canonical, previous)
		}
		a.mu.Unlock()
		return fmt.Errorf("could not save config: %w", sErr)
	}
	a.adoptLocked(canonical)
	a.mu.Unlock()

	a.rt.logger.Printf("Trusted folder added: %s", canonical)
	a.bringLive(canonical)
	return nil
}

// bringLive gives a newly trusted folder its own origin now rather than at the
// next launch. It does nothing when a site is already anchored there, which is
// adoptLocked's case.
//
// The recovery page tells the user to add the folder under Trusted Folders, and
// until 1.4.0 following that instruction changed nothing until a restart:
// trustFolder only adopted an existing SITE anchored at the folder, and a parked
// port is not one. So the bookmark kept answering with the same page that had just
// told them how to fix it. Binding here is the state startSites reaches at boot
// anyway, arrived at when the user asked for it.
//
// It fires on all three doors, not only the tray, because trustFolder is the
// common tail of the picker, the page banner, and the read prompt's Trust button.
// A page-steered approval therefore binds a listener and remembers a port for a
// folder no file has been opened in. That is intended, since the folder is
// genuinely trusted either way, but it is why sitePortCap matters more than it did.
func (a *app) bringLive(canonical string) {
	if !a.shouldBindLive(canonical) {
		return
	}

	// Release the recovery listener first, or the bind below finds its own port
	// taken and moves the origin, breaking the bookmark the port was held for.
	// Bind outside a.mu: it is the one call here that can block on a stalled mount.
	a.unpark(canonical)
	s, err := a.buildSite(canonical, true)
	if err != nil {
		a.rt.logger.Printf("Could not bind a port for trusted folder %s: %v", canonical, err)
		a.reparkRemembered(canonical)
		return
	}

	a.mu.Lock()
	// Re-check everything the first pass established. An untrust or a quit can land
	// while the bind is in flight, and a concurrent open can publish a site at this
	// anchor, so publishing on the earlier answer could leave a trusted site for a
	// folder no longer in the list, which the tray cannot show or revoke.
	if !a.shouldBindLiveLocked(canonical) {
		stopping := a.stopping
		a.mu.Unlock()
		s.close()
		// A quit wants nothing held. shutdown has already closed every parked
		// listener and emptied the list, so parking one now would bind a port
		// after the snapshot that was supposed to account for it.
		if !stopping {
			a.reparkRemembered(canonical)
		}
		return
	}
	a.sites = append(a.sites, s)
	// Persist the port in the same critical section that publishes the site, and
	// so under the same lock shutdown takes to set stopping. Saving after the
	// unlock let a config write land after shutdown had already returned.
	a.rememberPort(canonical, s.port)
	a.mu.Unlock()
	s.start(a.rt.logger)

	a.rt.logger.Printf("Trusted folder %s listening on 127.0.0.1:%d", canonical, s.port)
}

func (a *app) shouldBindLive(canonical string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.shouldBindLiveLocked(canonical)
}

// shouldBindLiveLocked answers whether canonical still wants a site of its own.
// Caller holds a.mu.
//
// The trustedAnchor test is invariant 4, exactly as startSites applies it: only
// the broadest of a nested set gets a site. Without it, trusting a folder inside
// one already trusted builds a second origin over a tree that already has one, and
// remembers a port for a shadowed anchor. The next startSites parks that port
// instead of serving it, so the same URL would work now and be dead after a
// restart, which is the class of bug this whole item exists to remove.
func (a *app) shouldBindLiveLocked(canonical string) bool {
	if a.stopping || a.siteAtLocked(canonical) != nil {
		return false
	}
	anchor, ok := a.trustedAnchor(canonical)
	return ok && anchor == canonical
}

// reparkRemembered puts a recovery page back on a port unparked for a bind that
// then did not happen, so a live bookmark degrades to a page that explains itself
// rather than to a refused connection for the rest of the run.
func (a *app) reparkRemembered(canonical string) {
	if port := a.rt.cfg.SitePort(canonical); port != 0 {
		a.parkPort(canonical, port)
	}
}

// adoptLocked brings a newly trusted folder live. Caller holds a.mu.
//
// This is all that remains of the three seeding loops. There is nothing to push
// into other sites: under broadest-wins anchoring a site holds at most its own
// anchor's trusted root (invariant 3), so the only site that can be affected is
// one anchored exactly here. That is also the common case, and the reason to
// handle it at all: the user opened a file, saw the read-only banner, and
// trusted that file's own folder, so the page they are looking at becomes
// editable on the same port and its address does not move.
//
// A site sitting strictly inside the new folder is deliberately left alone. Its
// files keep the tokens and the addresses they already have, and everything
// opened from now on anchors at the trusted folder, which is where the folder's
// own site gets built on the next open.
func (a *app) adoptLocked(canonical string) {
	s := a.siteAtLocked(canonical)
	if s == nil {
		return
	}
	s.trusted = true
	// Idempotent, so it does not matter whether the listener's lazy arm has
	// already fired for this site or is still to come.
	if err := s.sessions.InstallTrustedRoot(canonical); err != nil {
		a.rt.logger.Printf("Could not install trusted root %s: %v", canonical, err)
	}
}

// untrustFolder drops dir from the trusted list, closes its origin, and gives
// every file the user explicitly opened a new one.
//
// Closing the origin is what makes revocation real: the trusted root goes, the
// tokens minted under it die, and their live-sync streams are torn down. Files
// the user opened themselves survive that, because a double-click is its own
// capability and untrusting a folder was never meant to take it back; they are
// routed again, which lands them on a nested trusted folder if one covers them
// and on their own folder otherwise. That moves their port once, and it is why
// the tray asks before untrusting.
func (a *app) untrustFolder(dir string) error {
	a.mu.Lock()
	removed, ok := a.rt.cfg.RemoveTrustedFolder(dir)
	if !ok {
		a.mu.Unlock()
		return nil
	}
	// Forget the port in the same write that drops the trust, or re-homing below
	// binds it straight back and the survivor keeps the untrusted folder's origin.
	forgottenPort := a.rt.cfg.ForgetSitePort(dir)
	if err := a.rt.cfg.Save(); err != nil {
		// Restore what was taken out, exactly. Rebuilding the entry from the folder
		// on disk would re-pin a dead one to whatever is there now.
		a.rt.cfg.AddTrustedFolder(removed.Path, removed.Identity)
		if forgottenPort != 0 {
			a.rt.cfg.RememberSitePort(dir, forgottenPort)
		}
		a.mu.Unlock()
		return fmt.Errorf("could not save config: %w", err)
	}

	var dropped, reopen []string
	freedPort := 0
	if s := a.siteAtLocked(dir); s != nil {
		freedPort = s.port
		for _, reg := range s.sessions.Registrations() {
			dropped = append(dropped, reg.Path)
			if reg.Via.Has(session.ViaOsOpen) {
				reopen = append(reopen, reg.Path)
			}
		}
		s.close()
		a.removeSiteLocked(s)
	}
	// One "Trust this folder" click on the read prompt sets both the granted and
	// the trusted flag on a single read root, so clearing trust alone would leave
	// the granted flag holding the root open and the folder still readable.
	// Untrusting takes back everything that click gave. A folder the user
	// explicitly OPENED survives, because that root is what its page is served
	// from.
	for _, other := range a.sites {
		other.sessions.RevokeTrustedRoot(dir)
		other.sessions.RevokeReadRoot(dir)
	}
	a.mu.Unlock()

	for _, p := range dropped {
		a.rt.ls.DropSubscribers(p)
	}
	for _, p := range reopen {
		if _, _, ok := a.route(p, session.ViaOsOpen); !ok {
			a.rt.logger.Printf("Could not re-home %s after untrusting %s", p, dir)
		}
	}
	// Hold the freed port with the recovery page, so a live bookmark degrades to a
	// page rather than a connection refusal for the rest of this run. Re-homing
	// cannot have taken it back: the port was forgotten with the trust, so the
	// survivors bound fresh ones.
	if freedPort != 0 {
		a.parkPort(dir, freedPort)
	}

	a.rt.logger.Printf("Trusted folder removed: %s (%d registrations dropped, %d re-homed)", dir, len(dropped), len(reopen))
	return nil
}

// removeSiteLocked drops s from the registry. Caller holds a.mu and has already
// closed s.
func (a *app) removeSiteLocked(s *site) {
	kept := a.sites[:0]
	for _, other := range a.sites {
		if other != s {
			kept = append(kept, other)
		}
	}
	a.sites = kept
}

// mayTrustFromPrompt gates the read prompt's third button. The dialog does not
// offer to trust a folder that trustFromPrompt would then refuse: before this,
// the button was always drawn and the refusal arrived afterwards as a
// notification, which asked the user to make a choice that could not be honored.
func (a *app) mayTrustFromPrompt(dir string) bool {
	return !a.rt.policy.RefuseSteered(dir)
}

// trustFromPrompt records a folder on behalf of a permission dialog the user
// answered with "Trust this folder". It is deliberately stricter than the tray's
// own picker: here the PAGE chooses the folder being offered, by picking which
// out-of-scope assets to request, so it can aim the common ancestor at one of
// the personal folders where files the user never wrote tend to land, and offer
// one-click trust for the whole tree. Trusting one of those from the tray is
// still allowed, because that takes a deliberate act with a folder picker.
//
// The refusal runs again here even though mayTrustFromPrompt already suppressed
// the button, because this grants write and the gate is a UI decision made
// earlier, on a path the server owns.
//
// The caller has already installed the session read root, so refusing here costs
// only durability: the page keeps working now and asks again next launch. The
// user is told, because they asked for something permanent and got something
// temporary.
func (a *app) trustFromPrompt(dir string) error {
	if a.rt.policy.RefuseSteered(dir) {
		a.reportTrustRefused(dir, "A page picked this folder, and it is one of your main personal folders.")
		return fmt.Errorf("%s cannot be trusted from a permission prompt", dir)
	}
	if err := a.trustFolder(dir); err != nil {
		a.reportTrustRefused(dir, err.Error())
		return err
	}
	return nil
}

// trustFromPage handles a page's request to trust its own folder. It is the one
// door behind both the read-only banner's button and an editable page's own
// request, because after the banner started trusting a folder rather than
// opening a single file, the two became the same act reached through different
// front doors (a nonce and a token).
//
// The folder is derived HERE from the requesting file: the page never names one,
// so it cannot steer the choice, and the looser refusal rule is the correct one.
// The server has already required that the user acted on the file, either from
// the desktop or through the banner. The dialog names the requesting file and
// the full folder path, states the consequence in full, and says up front when
// the file was reached by a link.
func (a *app) trustFromPage(requestingFile string, openedByUser bool) (string, bool) {
	// Already inside a trusted folder: trusting again grants nothing, so answer
	// with where the file serves and raise no dialog. A page that asks on every
	// load prompts once, not once per reload.
	if _, trusted := a.anchorFor(requestingFile); trusted {
		return a.serveURL(requestingFile)
	}

	folder := filepath.Dir(requestingFile)
	canonical, err := a.rt.policy.Canonical(folder)
	if err != nil {
		a.rt.logger.Printf("Trust request for %s refused: %v", folder, err)
		return "", false
	}
	if a.rt.policy.RefuseOwnFolder(canonical) {
		a.rt.logger.Printf("Trust request refused for protected folder %s", canonical)
		a.reportPageTrustRefused(canonical)
		return "", false
	}

	reached := ""
	if !openedByUser {
		reached = "You reached this file from a link rather than opening it yourself.\n\n"
	}
	msg := fmt.Sprintf("%s%s wants to trust its folder:\n\n%s\n\nEvery HTML Clay file in that folder becomes editable without asking, including files added later, and any file in it will be able to change any other. Only allow this for a folder you control completely.",
		reached, requestingFile, canonical)
	allowed, err := a.confirmTrustRequest("HTML Clay", msg, "Trust Folder")
	if err != nil {
		a.rt.logger.Printf("Trust dialog error for %s: %v", canonical, err)
		return "", false
	}
	if !allowed {
		return "", false
	}
	if err := a.trustFolder(canonical); err != nil {
		a.rt.logger.Printf("Could not trust folder %s: %v", canonical, err)
		return "", false
	}
	a.rt.logger.Printf("Folder %s trusted from a page request by %s", canonical, requestingFile)
	return a.serveURL(requestingFile)
}

// serveURL routes absPath and reports where it is served. A file already
// registered keeps its origin; one that is not gets the origin its anchor now
// implies, which after a trust is the trusted folder's.
func (a *app) serveURL(absPath string) (string, bool) {
	s, rel, ok := a.route(absPath, session.ViaTrusted)
	if !ok {
		return "", false
	}
	return fileURL(s.port, rel), true
}

// confirmTrustRequest raises a two-button dialog through its test seam,
// defaulting to the real native one. It is a separate seam from the read
// prompt's confirm so a test that approves one class of dialog can never
// silently approve the other.
//
// affirmative is what the button the user clicks actually says, and it is a
// parameter because this helper serves both directions: the same dialog asks to
// trust a folder and to stop trusting one. A fixed "Trust Folder" label shipped
// on the removal dialog in 1.3.0, so "Stop trusting this folder?" was answered
// by clicking a button that said Trust Folder, and Deny was the one that kept
// the trust. Every label was the opposite of its action.
func (a *app) confirmTrustRequest(title, message, affirmative string) (bool, error) {
	if a.rt.confirmTrust != nil {
		return a.rt.confirmTrust(title, message, affirmative)
	}
	return platform.ConfirmWithButtons(title, message, affirmative)
}

// reportTrustRefused tells the user why a folder was not remembered. Staying
// silent is its own bug: they asked for something durable and got something that
// lasts until they quit, so without this the folder simply starts asking again
// with no explanation. It covers ordinary failures too, such as a config file
// that cannot be written, not only the protected-folder policy refusal.
//
// The notification is detached because this runs on the broker's prompt goroutine,
// which must not block: a notification that wedges (on Windows it is a modal that
// waits to be clicked) would leave the broker marked as prompting, and the site
// would never raise another permission dialog for the rest of its life.
func (a *app) reportTrustRefused(dir, reason string) {
	msg := dir + " was not added to your trusted folders.\n\n" + reason +
		"\n\nIt stays readable until you quit. You can add it yourself from the HTML Clay menu."
	go func() {
		if nErr := a.notifyUser("HTML Clay", msg); nErr != nil {
			a.rt.logger.Printf("Could not notify about the refused trust of %s: %v", dir, nErr)
		}
	}()
}

// reportPageTrustRefused tells the user a page asked for a protected folder.
// Detached for the same reason as reportTrustRefused: this runs on a prompt path
// that must never block.
func (a *app) reportPageTrustRefused(dir string) {
	msg := dir + " was not trusted.\n\nA page requested it, and it is one of your main personal folders. You can add a trusted folder yourself from the HTML Clay menu."
	go func() {
		if nErr := a.notifyUser("HTML Clay", msg); nErr != nil {
			a.rt.logger.Printf("Could not notify about the refused trust of %s: %v", dir, nErr)
		}
	}()
}

// trustedFolderRows returns the tray's rows. The label carries the dead marker
// and the path is carried alongside it, so removal never has to recover a path
// by stripping a suffix off a label the user might have produced themselves by
// naming a folder that way.
func (a *app) trustedFolderRows() []tray.Row {
	list := a.rt.cfg.TrustedFolderList()
	out := make([]tray.Row, 0, len(list))
	for _, tf := range list {
		label := tf.Path
		if info, err := os.Stat(tf.Path); err != nil || !info.IsDir() || !trust.IdentityOK(tf.Path, tf.Identity) {
			// The entry stays listed: it is the record of a standing write grant,
			// and it must surface as dead rather than silently vanish.
			label += " (missing or replaced)"
		} else if outer, ok := a.shadowedBy(tf.Path); ok {
			// Dead wins over covered when both apply: a dead entry grants nothing
			// whatever encloses it.
			label += " (covered by " + filepath.Base(outer) + ")"
		}
		out = append(out, tray.Row{Path: tf.Path, Label: label})
	}
	return out
}

// shadowedBy names the live trusted folder that owns dir's tree, when that is not
// dir itself.
//
// Broadest-wins means an enclosing trusted folder anchors everything under it, so
// the inner entry is a declaration with no present effect: untrusting it closes
// nothing and its files stay editable. The tray said the opposite, promising that
// files would stop opening editable and open pages would need reopening, and a
// brand new file created in the folder afterwards still served with a save token.
//
// It asks trustedAnchor rather than testing containment lexically, so an outer
// folder whose identity pin no longer matches does not count: the inner entry is
// then genuinely in charge and its removal really does revoke.
func (a *app) shadowedBy(dir string) (string, bool) {
	anchor, ok := a.trustedAnchor(dir)
	if !ok || anchor == dir {
		return "", false
	}
	return anchor, true
}

// pickAndTrustFolder pops the native folder picker and trusts the choice. The
// picker route deliberately skips the page-request refusal list: a deliberate
// act with a folder picker may choose anything Canonical accepts. It does warn
// once for a personal folder, because picking ~/Documents by accident from a
// file dialog is easy and the consequence is the whole tree.
func (a *app) pickAndTrustFolder() []tray.Row {
	dir, ok, err := platform.SelectFolder("Choose a folder to trust. Every HTML Clay file inside it (including files added later) opens editable with no prompts, and any file in it can change any other.")
	if err != nil {
		a.rt.logger.Printf("Folder picker failed: %v", err)
		return a.trustedFolderRows()
	}
	if !ok {
		return a.trustedFolderRows()
	}
	canonical, cErr := a.rt.policy.Canonical(dir)
	if cErr == nil && a.rt.policy.IsPersonal(canonical) {
		msg := fmt.Sprintf("%s is one of your main personal folders.\n\nTrusting it makes every HTML Clay file anywhere inside it editable with no prompts, and lets any of them change any other. Most people want a single project folder instead.", canonical)
		proceed, cfErr := a.confirmTrustRequest("HTML Clay", msg, "Trust Folder")
		if cfErr != nil || !proceed {
			return a.trustedFolderRows()
		}
	}
	if cErr == nil {
		cErr = a.trustFolder(canonical)
	}
	if cErr != nil {
		a.rt.logger.Printf("Could not trust folder %s: %v", dir, cErr)
		go func() {
			if nErr := a.notifyUser("HTML Clay can't trust this folder", cErr.Error()); nErr != nil {
				a.rt.logger.Printf("Could not show notification: %v", nErr)
			}
		}()
	}
	return a.trustedFolderRows()
}

// removeTrustedFolder is the tray's remove hook. It asks first, because a
// misclick now closes a live origin and kills the address a bookmark points at.
//
// A folder covered by a broader trusted folder gets different wording, because
// for it none of that is true: the outer folder still anchors the tree, so nothing
// closes and nothing stops opening editable. Removing it is still worth allowing,
// since it is the user's own declaration and it takes effect the moment the outer
// folder goes, but the dialog must not claim a revocation it will not perform.
func (a *app) removeTrustedFolder(dir string) []tray.Row {
	msg := fmt.Sprintf("Stop trusting this folder?\n\n%s\n\nFiles in it will stop opening editable, and any of its pages you have open now will need to be opened again.", dir)
	affirmative := "Stop Trusting"
	if outer, ok := a.shadowedBy(dir); ok {
		msg = fmt.Sprintf("Remove this folder from your trusted list?\n\n%s\n\nIt sits inside %s, which you also trust, so its files stay editable and its pages stay open. Removing it here takes effect if you stop trusting that folder too.", dir, outer)
		affirmative = "Remove"
	}
	proceed, err := a.confirmTrustRequest("HTML Clay", msg, affirmative)
	if err != nil || !proceed {
		return a.trustedFolderRows()
	}
	if err := a.untrustFolder(dir); err != nil {
		a.rt.logger.Printf("Could not untrust folder %s: %v", dir, err)
	}
	return a.trustedFolderRows()
}

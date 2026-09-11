package config

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"sort"
	"time"
)

const (
	helperProgramCap  = 32
	helperDecisionCap = 512
)

var (
	ErrHelperNameInvalid   = errors.New("invalid helper name")
	ErrHelperRegistryFull  = errors.New("helper program registry is full")
	ErrHelperDecisionsFull = errors.New("helper decision registry is full")
)

// HelperProgram is one registered program. ID is a stable opaque local id and is
// what a decision points at. Name is a display name and is not unique: two
// registrations may share one because each document binds the name to a program.
//
// Path is stored as selected and is never resolved through EvalSymlinks. A
// program installed behind a stable symlink must pick up package manager updates.
type HelperProgram struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Path        string `json:"path"`
	AnyDocument bool   `json:"anyDocument,omitempty"`
	AddedAt     int64  `json:"addedAt"`
}

// HelperDecision is one document's answer about one name, allow or deny.
// Denials are stored so a refusal sticks until it is changed in the tray.
// Program is the registration ID and is empty on a denial, so permission cannot
// pass to a later registration that merely reuses the display name.
type HelperDecision struct {
	Document  string `json:"document"`
	Name      string `json:"name"`
	Program   string `json:"program,omitempty"`
	Allowed   bool   `json:"allowed"`
	DecidedAt int64  `json:"decidedAt"`
}

type HelperResolution struct {
	Decided bool
	Allowed bool
	Program HelperProgram
}

// ValidHelperName reports whether s is lowercase ASCII letters, digits, and
// hyphens, from 1 to 32 bytes, with no leading or trailing hyphen. A document
// supplies this text to a native permission prompt, so it is bounded here.
func ValidHelperName(s string) bool {
	if len(s) == 0 || len(s) > 32 || s[0] == '-' || s[len(s)-1] == '-' {
		return false
	}
	for i := 0; i < len(s); i++ {
		if (s[i] < 'a' || s[i] > 'z') && (s[i] < '0' || s[i] > '9') && s[i] != '-' {
			return false
		}
	}
	return true
}

// Canonical ordering lets a rollback restore the exact bytes Save wrote without
// exposing mutable slice indexes through the transaction API.
func helperProgramLess(a, b HelperProgram) bool {
	if a.AddedAt != b.AddedAt {
		return a.AddedAt < b.AddedAt
	}
	return a.ID < b.ID
}

func helperDecisionLess(a, b HelperDecision) bool {
	if a.DecidedAt != b.DecidedAt {
		return a.DecidedAt < b.DecidedAt
	}
	if a.Document != b.Document {
		return a.Document < b.Document
	}
	return a.Name < b.Name
}

func sortHelperPrograms(programs []HelperProgram) {
	sort.SliceStable(programs, func(i, j int) bool {
		return helperProgramLess(programs[i], programs[j])
	})
}

func sortHelperDecisions(decisions []HelperDecision) {
	sort.SliceStable(decisions, func(i, j int) bool {
		return helperDecisionLess(decisions[i], decisions[j])
	})
}

func newHelperProgramID(programs []HelperProgram) (string, error) {
	for {
		var raw [16]byte
		if _, err := rand.Read(raw[:]); err != nil {
			return "", fmt.Errorf("cannot generate helper program id: %w", err)
		}
		id := hex.EncodeToString(raw[:])
		unique := true
		for _, p := range programs {
			if p.ID == id {
				unique = false
				break
			}
		}
		if unique {
			return id, nil
		}
	}
}

func (c *Config) AddHelperProgram(name, path string) (HelperProgram, error) {
	if !ValidHelperName(name) {
		return HelperProgram{}, ErrHelperNameInvalid
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.HelperPrograms) >= helperProgramCap {
		return HelperProgram{}, ErrHelperRegistryFull
	}
	id, err := newHelperProgramID(c.HelperPrograms)
	if err != nil {
		return HelperProgram{}, err
	}
	// ResolveHelper proposes the earliest registration of a name, and two
	// registrations inside one clock tick would otherwise be ordered by their
	// random IDs. Windows advances the wall clock once per interrupt.
	addedAt := time.Now().UnixNano()
	for _, existing := range c.HelperPrograms {
		if existing.AddedAt >= addedAt {
			addedAt = existing.AddedAt + 1
		}
	}
	p := HelperProgram{ID: id, Name: name, Path: path, AddedAt: addedAt}
	c.HelperPrograms = append(c.HelperPrograms, p)
	sortHelperPrograms(c.HelperPrograms)
	return p, nil
}

func (c *Config) RestoreHelperProgram(p HelperProgram) bool {
	if p.ID == "" || !ValidHelperName(p.Name) {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.HelperPrograms) >= helperProgramCap {
		return false
	}
	for _, existing := range c.HelperPrograms {
		if existing.ID == p.ID {
			return false
		}
	}
	c.HelperPrograms = append(c.HelperPrograms, p)
	sortHelperPrograms(c.HelperPrograms)
	return true
}

// RemoveHelperProgram unregisters a program and leaves every decision row alone,
// including the rows that pointed at it. ResolveHelper already reads an allowed
// decision whose program is gone as undecided, so those documents ask again at
// next open, and re-registering the same ID restores them.
//
// Deleting them here would be the wrong shape twice over. Forgetting decisions
// is its own tray action (ForgetHelperProgramDecisions), so folding it into
// removal makes one menu item silently do two things; and a refusal is meant to
// be permanent until the user changes it in the tray, which a removal that
// clears rows would quietly undo.
func (c *Config) RemoveHelperProgram(id string) (HelperProgram, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for i, p := range c.HelperPrograms {
		if p.ID != id {
			continue
		}
		c.HelperPrograms = append(c.HelperPrograms[:i], c.HelperPrograms[i+1:]...)
		return p, true
	}
	return HelperProgram{}, false
}

func (c *Config) SetHelperAnyDocument(id string, v bool) (previous bool, ok bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for i, p := range c.HelperPrograms {
		if p.ID == id {
			previous = p.AnyDocument
			c.HelperPrograms[i].AnyDocument = v
			return previous, true
		}
	}
	return false, false
}

func (c *Config) HelperProgramList() []HelperProgram {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]HelperProgram(nil), c.HelperPrograms...)
}

func (c *Config) LookupHelperProgram(id string) (HelperProgram, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, p := range c.HelperPrograms {
		if p.ID == id {
			return p, true
		}
	}
	return HelperProgram{}, false
}

func (c *Config) DecideHelper(d HelperDecision) (previous HelperDecision, had bool, err error) {
	if !ValidHelperName(d.Name) {
		return HelperDecision{}, false, ErrHelperNameInvalid
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if d.Allowed {
		found := false
		for _, p := range c.HelperPrograms {
			if p.ID == d.Program {
				found = true
				break
			}
		}
		if !found {
			return HelperDecision{}, false, fmt.Errorf("helper program %q is not registered", d.Program)
		}
	} else {
		d.Program = ""
	}
	for i, existing := range c.HelperDecisions {
		if existing.Document == d.Document && existing.Name == d.Name {
			c.HelperDecisions[i] = d
			sortHelperDecisions(c.HelperDecisions)
			return existing, true, nil
		}
	}
	if len(c.HelperDecisions) >= helperDecisionCap {
		return HelperDecision{}, false, ErrHelperDecisionsFull
	}
	c.HelperDecisions = append(c.HelperDecisions, d)
	sortHelperDecisions(c.HelperDecisions)
	return HelperDecision{}, false, nil
}

func (c *Config) RestoreHelperDecisions(ds []HelperDecision) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	restored := 0
	for _, d := range ds {
		// An allowed row naming an unregistered program is restorable, because
		// RemoveHelperProgram and normalizeHelpers both keep exactly that row.
		// Refusing it here made rollback lossy in the one case it exists for: a
		// failed approval that replaced such a row deleted the replacement and
		// then could not put the original back. Allowed with NO program is still
		// refused, because that row is malformed rather than stale.
		if len(c.HelperDecisions) >= helperDecisionCap || !ValidHelperName(d.Name) ||
			(d.Allowed && d.Program == "") || (!d.Allowed && d.Program != "") {
			continue
		}
		duplicate := false
		for _, existing := range c.HelperDecisions {
			if existing.Document == d.Document && existing.Name == d.Name {
				duplicate = true
				break
			}
		}
		if duplicate {
			continue
		}
		c.HelperDecisions = append(c.HelperDecisions, d)
		restored++
	}
	sortHelperDecisions(c.HelperDecisions)
	return restored
}

func (c *Config) ForgetHelperDecision(document, name string) (HelperDecision, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for i, d := range c.HelperDecisions {
		if d.Document == document && d.Name == name {
			c.HelperDecisions = append(c.HelperDecisions[:i], c.HelperDecisions[i+1:]...)
			return d, true
		}
	}
	return HelperDecision{}, false
}

func (c *Config) ForgetHelperProgramDecisions(id string) []HelperDecision {
	c.mu.Lock()
	defer c.mu.Unlock()
	kept := make([]HelperDecision, 0, len(c.HelperDecisions))
	removed := make([]HelperDecision, 0)
	for _, d := range c.HelperDecisions {
		if d.Program == id {
			removed = append(removed, d)
		} else {
			kept = append(kept, d)
		}
	}
	c.HelperDecisions = kept
	return removed
}

func (c *Config) HelperDecisionList() []HelperDecision {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]HelperDecision(nil), c.HelperDecisions...)
}

// ResolveHelper answers in one locked read whether there is a decision, whether
// it allows execution, and which registered program it selects.
func (c *Config) ResolveHelper(document, name string) (HelperResolution, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, d := range c.HelperDecisions {
		if d.Document != document || d.Name != name {
			continue
		}
		if !d.Allowed {
			return HelperResolution{Decided: true}, true
		}
		for _, p := range c.HelperPrograms {
			if p.ID == d.Program {
				return HelperResolution{Decided: true, Allowed: true, Program: p}, true
			}
		}
		break
	}
	var candidate HelperProgram
	for _, p := range c.HelperPrograms {
		if p.Name != name {
			continue
		}
		if p.AnyDocument {
			return HelperResolution{Decided: true, Allowed: true, Program: p}, true
		}
		if candidate.ID == "" {
			candidate = p
		}
	}
	if candidate.ID != "" {
		return HelperResolution{Program: candidate}, true
	}
	return HelperResolution{}, false
}

func (c *Config) normalizeHelpers() (droppedPrograms, droppedDecisions int) {
	c.mu.Lock()
	defer c.mu.Unlock()

	programs := make([]HelperProgram, 0, len(c.HelperPrograms))
	programIDs := make(map[string]bool, len(c.HelperPrograms))
	for _, p := range c.HelperPrograms {
		if p.ID == "" || !ValidHelperName(p.Name) || programIDs[p.ID] || len(programs) >= helperProgramCap {
			droppedPrograms++
			continue
		}
		programIDs[p.ID] = true
		programs = append(programs, p)
	}
	sortHelperPrograms(programs)
	c.HelperPrograms = programs

	type decisionKey struct {
		document string
		name     string
	}
	decisions := make([]HelperDecision, 0, len(c.HelperDecisions))
	decisionIndexes := make(map[decisionKey]int, len(c.HelperDecisions))
	for _, d := range c.HelperDecisions {
		// A decision naming a program that is not registered is kept, not dropped.
		// ResolveHelper already reads it as undecided, so the document asks again
		// and the answer replaces this row in place; dropping it here would make
		// the load path disagree with RemoveHelperProgram, which leaves the same
		// rows alone. An allowed decision naming NO program is different: it is
		// malformed rather than stale, and there is nothing for a later answer to
		// key off.
		if !ValidHelperName(d.Name) || (d.Allowed && d.Program == "") {
			droppedDecisions++
			continue
		}
		if !d.Allowed {
			d.Program = ""
		}
		key := decisionKey{document: d.Document, name: d.Name}
		if i, duplicate := decisionIndexes[key]; duplicate {
			droppedDecisions++
			if d.DecidedAt > decisions[i].DecidedAt {
				decisions[i] = d
			}
			continue
		}
		decisionIndexes[key] = len(decisions)
		decisions = append(decisions, d)
	}
	if over := len(decisions) - helperDecisionCap; over > 0 {
		type eviction struct {
			index   int
			missing bool
		}
		evictions := make([]eviction, len(decisions))
		for i, d := range decisions {
			_, statErr := os.Stat(d.Document)
			evictions[i] = eviction{index: i, missing: os.IsNotExist(statErr)}
		}
		sort.SliceStable(evictions, func(i, j int) bool {
			a, b := evictions[i], evictions[j]
			if a.missing != b.missing {
				return a.missing
			}
			return helperDecisionLess(decisions[a.index], decisions[b.index])
		})
		remove := make(map[int]bool, over)
		for _, e := range evictions[:over] {
			remove[e.index] = true
		}
		kept := make([]HelperDecision, 0, helperDecisionCap)
		for i, d := range decisions {
			if remove[i] {
				droppedDecisions++
			} else {
				kept = append(kept, d)
			}
		}
		decisions = kept
	}
	sortHelperDecisions(decisions)
	c.HelperDecisions = decisions
	return droppedPrograms, droppedDecisions
}

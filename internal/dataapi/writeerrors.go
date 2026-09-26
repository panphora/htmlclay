package dataapi

import (
	"fmt"
)

// NoRulesTag means the document publishes no rules tag for the token the request named.
type NoRulesTag struct {
	Token string `json:"token"`
}

func (e *NoRulesTag) Error() string {
	return fmt.Sprintf("no <script data-rules-name~=%q> rules tag in this document", e.Token)
}

// Unmatched names a scalar rule whose selector matched nothing, with the path label that reaches it.
type Unmatched struct {
	Path     string `json:"path"`
	Selector string `json:"selector"`
}

// WriteRejected means the plan refused the body before anything was written: keys the rules do not
// define, or scalar rules no element answers.
type WriteRejected struct {
	UnknownKeys []string    `json:"unknownKeys"`
	Unmatched   []Unmatched `json:"unmatched"`
}

func (e *WriteRejected) Error() string {
	return fmt.Sprintf("write rejected: %d unknown key(s), %d rule(s) with no matching element",
		len(e.UnknownKeys), len(e.Unmatched))
}

// Refusal is one write the content-only policy declined, and why.
type Refusal struct {
	Target string `json:"target"`
	Reason string `json:"reason"`
}

// WriteRefused carries every refusal from one body, in write order. The engine throws it once,
// rather than on the first refusal, so a caller sees all of them at the same time.
type WriteRefused struct {
	Refusals []Refusal `json:"refusals"`
}

func (e *WriteRefused) Error() string {
	return fmt.Sprintf("write refused: %d write(s) broke the content-only policy", len(e.Refusals))
}

// Mismatch is one field of the body whose shape the rules do not accept.
type Mismatch struct {
	Path     string `json:"path"`
	Expected string `json:"expected"`
	Got      string `json:"got"`
}

// ShapeMismatch means the body does not fit the rule tree, checked before any write.
type ShapeMismatch struct {
	Mismatches []Mismatch `json:"mismatches"`
}

func (e *ShapeMismatch) Error() string {
	return fmt.Sprintf("shape mismatch: %d field(s) failed validation", len(e.Mismatches))
}

// EmptyListInsert means the body adds to a list that has no row to clone and no [cms-template] seed
// to grow from. Path is the write path that reached it, with int list indices and string object
// keys, so it marshals the way the reference's mixed-type path does.
type EmptyListInsert struct {
	Path []any `json:"path"`
}

func (e *EmptyListInsert) Error() string {
	return "cannot add items to empty list at \"" + pathString(e.Path) +
		"\" \u2014 no sibling to clone as template. Seed the list with a hidden item first."
}

// RuleTargetReadOnly means the rule names a DOM property the engine refuses to write at all.
type RuleTargetReadOnly struct {
	Name string `json:"target"`
}

func (e *RuleTargetReadOnly) Error() string {
	return fmt.Sprintf("cannot write to read-only DOM property %q", e.Name)
}

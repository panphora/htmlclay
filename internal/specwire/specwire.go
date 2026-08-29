// Package specwire implements the parts of the Malleable HTML File wire contract
// that more than one lane has to agree on.
//
// It is the Go twin of hyperclay's server-lib/spec-wire.js, and the two must stay
// byte-compatible. An etag is a promise across hosts, not within one: a document
// moved between hyperclay and HTML Clay carries the stamp it last saw, and if the
// two disagree about how a stamp is computed, every such save is refused with a
// conflict nobody can explain. Change either implementation and change both.
package specwire

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// Etag stamps the bytes a host STORED, never the bytes it was sent (spec §6).
//
// The two differ whenever a host normalises before writing, and stamping what was
// sent would tell a client its disk holds something it does not. On this host the
// difference is real: a save arrives carrying a token attribute and a possible
// banner, both stripped before the write, so the stamp is taken from the stripped
// bytes.
//
// sha256, hex, first 16 characters, matching hyperclay's documentEtag exactly.
func Etag(stored []byte) string {
	sum := sha256.Sum256(stored)
	return hex.EncodeToString(sum[:])[:16]
}

// IfMatchSatisfied reports whether an If-Match field value permits writing over
// these stored bytes.
//
// Our own clients and the conformance page send one bare stamp, but RFC 9110
// §13.1.1 spells the field as `*` or a comma-separated list of entity-tags, each
// optionally quoted and optionally weak. A third-party client on a public spec
// that simply follows HTTP would otherwise get a 412 it could never explain.
//
// `*` asks only that the document exist at all. Weak tags count as matches, which
// is looser than the RFC's strong comparison and deliberate: the stamp is a digest
// of the exact stored bytes, so there is no weak form of it to confuse it with.
//
// Callers pass the stored bytes rather than a stamp, so `*` can be answered
// without inventing a second convention for "the document is empty" versus "there
// is no document". An empty or absent field value matches nothing, so a client
// that computed its stamp wrong is refused rather than quietly dropped back to
// last-write-wins.
func IfMatchSatisfied(fieldValue string, stored []byte) bool {
	field := strings.TrimSpace(fieldValue)
	if field == "*" {
		return len(stored) > 0
	}

	current := Etag(stored)
	for _, entry := range strings.Split(field, ",") {
		tag := strings.TrimSpace(entry)
		if len(tag) >= 2 && strings.EqualFold(tag[:2], "W/") {
			tag = tag[2:]
		}
		if len(tag) >= 2 && strings.HasPrefix(tag, `"`) && strings.HasSuffix(tag, `"`) {
			tag = tag[1 : len(tag)-1]
		}
		if tag != "" && tag == current {
			return true
		}
	}
	return false
}

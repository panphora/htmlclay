package dataapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/net/html"
)

// TestConformanceWrite is the write face of the corpus: every case whose meta says `face: write` is
// a JSON body posted through the document's own rules tag. It compares the same things the
// reference's writeDocument promises — the exact bytes written, whether they were spliced into the
// source or rendered whole, a fresh extraction of them, and, for a refusal, the type, message and
// structured details.
func TestConformanceWrite(t *testing.T) {
	var matched, errored, skipped int

	for _, name := range caseNames(t) {
		meta := parseMeta(t, filepath.Join(corpusDir, "cases", name+".meta"))
		if meta.face != "write" {
			continue
		}

		t.Run(name, func(t *testing.T) {
			if r := meta.skipReason(); r != "" {
				skipped++
				t.Skip(r)
			}
			source, ok := readCaseFile(t, name, ".html")
			if !ok {
				t.Fatalf("%s has no .html", name)
			}
			raw, ok := readCaseFile(t, name, ".data.json")
			if !ok {
				t.Fatalf("%s has no .data.json", name)
			}
			data, err := decodeJSON(raw)
			if err != nil {
				t.Fatalf("%s.data.json: %v", name, err)
			}

			result, err := WriteDocument([]byte(source), data, meta.token)

			if meta.expect == "error" {
				if err == nil {
					t.Fatalf("WriteDocument succeeded, but the reference produced %s",
						wantErrorType(t, name))
				}
				checkWriteError(t, name, err)
				errored++
				return
			}
			if err != nil {
				t.Fatalf("WriteDocument: %v", err)
			}
			checkWriteResult(t, name, meta.token, result)
			matched++
		})
	}

	t.Logf("write: %d matched, %d errored as expected, %d skipped", matched, errored, skipped)
	if matched+errored == 0 {
		t.Fatal("no write cases ran \u2014 the corpus is not wired up")
	}
}

// checkWriteResult compares the written document three ways: the exact bytes, whether they were
// spliced, and the extraction a reader of those bytes would see. The bytes are the whole point of
// the splice — everything the write did not touch has to be unchanged, CRLF included — so they are
// compared first and verbatim.
func checkWriteResult(t *testing.T, name, token string, result *WriteResult) {
	t.Helper()

	rawWrite, ok := readCaseFile(t, name, ".write.json")
	if !ok {
		t.Fatalf("%s has no .write.json", name)
	}
	var want struct {
		Changed bool `json:"changed"`
		Spliced bool `json:"spliced"`
	}
	if err := json.Unmarshal([]byte(rawWrite), &want); err != nil {
		t.Fatalf("%s.write.json: %v", name, err)
	}
	if result.Changed != want.Changed {
		t.Errorf("changed = %v, want %v", result.Changed, want.Changed)
	}
	if result.Spliced != want.Spliced {
		t.Errorf("spliced = %v, want %v", result.Spliced, want.Spliced)
	}

	after, ok := readCaseFile(t, name, ".after.html")
	if !ok {
		t.Fatalf("%s has no .after.html", name)
	}
	if string(result.HTML) != after {
		t.Errorf("bytes:\n got: %q\nwant: %q", result.HTML, after)
	}

	d, err := ParseBytes(result.HTML)
	if err != nil {
		t.Fatalf("reparse written output: %v", err)
	}
	found, err := d.FindRulesIn(token)
	if err != nil {
		t.Fatalf("written output: %v", err)
	}
	if found == nil {
		t.Fatalf("written output has no rules tag for %q", token)
	}
	got, err := d.Extract(found.Rules)
	if err != nil {
		t.Fatalf("Extract on written output: %v", err)
	}
	wantAfter, ok := readCaseFile(t, name, ".after.json")
	if !ok {
		t.Fatalf("%s has no .after.json", name)
	}
	assertSameJSON(t, name, got, wantAfter)
}

// checkWriteError compares type, message text and the structured details a host puts in its body.
// The message is compared here, unlike the extraction test: these messages are pinned by the
// corpus and every one of them is written for an author to read.
func checkWriteError(t *testing.T, name string, err error) {
	t.Helper()

	raw, ok := readCaseFile(t, name, ".error.json")
	if !ok {
		t.Fatalf("%s has no .error.json", name)
	}
	tree, derr := decodeJSON(raw)
	if derr != nil {
		t.Fatalf("%s.error.json: %v", name, derr)
	}
	want, ok := tree.(*Object)
	if !ok {
		t.Fatalf("%s.error.json is not an object", name)
	}

	wantType, _ := want.Get("type")
	if got := writeErrorName(err); got != wantType {
		t.Fatalf("error type %q, want %v (from %v)", got, wantType, err)
	}
	wantMessage, _ := want.Get("message")
	if got := err.Error(); got != wantMessage {
		t.Errorf("message = %q, want %v", got, wantMessage)
	}

	wantDetails, _ := want.Get("details")
	gotJSON, err := Marshal(writeErrorDetails(err))
	if err != nil {
		t.Fatalf("Marshal(got details): %v", err)
	}
	wantJSON, err := Marshal(wantDetails)
	if err != nil {
		t.Fatalf("Marshal(want details): %v", err)
	}
	if string(gotJSON) != string(wantJSON) {
		t.Errorf("details:\n got: %s\nwant: %s", gotJSON, wantJSON)
	}
}

// writeErrorName maps a Go error onto the JS `err.name` the corpus recorded, for the types this
// face can raise.
func writeErrorName(err error) string {
	var (
		noRules  *NoRulesTag
		rejected *WriteRejected
		refused  *WriteRefused
		shape    *ShapeMismatch
		empty    *EmptyListInsert
		readOnly *RuleTargetReadOnly
	)
	switch {
	case errors.As(err, &noRules):
		return "NoRulesTag"
	case errors.As(err, &rejected):
		return "WriteRejected"
	case errors.As(err, &refused):
		return "WriteRefused"
	case errors.As(err, &shape):
		return "ShapeMismatch"
	case errors.As(err, &empty):
		return "EmptyListInsert"
	case errors.As(err, &readOnly):
		return "RuleTargetReadOnly"
	}
	return fmt.Sprintf("%T", err)
}

// writeErrorDetails is the host-body half of each error, mirroring conformance.mjs's writeDetails.
func writeErrorDetails(err error) Value {
	var rejected *WriteRejected
	if errors.As(err, &rejected) {
		return rejected
	}
	var refused *WriteRefused
	if errors.As(err, &refused) {
		return refused.Refusals
	}
	var shape *ShapeMismatch
	if errors.As(err, &shape) {
		return shape.Mismatches
	}
	var empty *EmptyListInsert
	if errors.As(err, &empty) {
		return empty.Path
	}
	return nil
}

// FuzzSpliceNeverLies is the invariant the splice is allowed to rest on: whatever bytes it produces,
// reparsing them and rendering them again has to reproduce the tree the write mutated exactly. A
// span that is off by a byte, or a token paired with the wrong element, would otherwise be free to
// silently damage bytes the write never touched.
//
// The seed corpus is every write case's source plus the documents the offsets are pinned against.
func FuzzSpliceNeverLies(f *testing.F) {
	metas, err := filepath.Glob(filepath.Join(corpusDir, "cases", "*.meta"))
	if err != nil {
		f.Fatal(err)
	}
	for _, meta := range metas {
		src, err := os.ReadFile(strings.TrimSuffix(meta, ".meta") + ".html")
		if err == nil {
			f.Add(src)
		}
	}
	for _, doc := range offsetDocs {
		f.Add([]byte(doc.src))
	}

	f.Fuzz(func(t *testing.T, src []byte) {
		d, err := ParseBytes(src)
		if err != nil {
			// The parser refuses some inputs outright (an open stack deeper than its
			// limit, for one). There is no tree, so there is nothing to splice; the
			// write path reports that error instead of producing bytes at all.
			return
		}
		spans, owner := sourceSpans(d, src)

		var targets []*html.Node
		for _, n := range sourceOrderElements(d) {
			targets = append(targets, n)
		}
		if len(targets) == 0 || len(src) == 0 {
			// No element to mutate. A document with no elements, an empty input, and one the
			// parser refused all leave nothing to splice.
			return
		}
		// Any element will do, with or without a span: an element that failed to pair keeps the
		// splice honest too, because splice falls back to the whole-document render for it. The
		// input's first byte picks which, so one corpus entry exercises more than one over the
		// course of a run.
		target := targets[int(src[0])%len(targets)]

		setAttr(target, "data-x", "1")
		w := &writer{
			d:      d,
			dirty:  map[*html.Node]bool{target: true},
			cloned: map[*html.Node]bool{},
			spans:  spans,
			owner:  owner,
		}
		full := []byte(outerHTML(d, d.Root))

		result := w.splice(src, full)

		got, err := ParseBytes(result.HTML)
		if err != nil {
			t.Fatalf("reparse spliced output: %v", err)
		}
		if rendered := outerHTML(got, got.Root); rendered != string(full) {
			t.Fatalf("splice changed the document: spliced = %v\n src: %q\n out: %q\n got: %q\nwant: %q",
				result.Spliced, src, result.HTML, rendered, full)
		}
	})
}

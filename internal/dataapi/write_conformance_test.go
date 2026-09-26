package dataapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
)

// TestConformanceWrite is the write face of the corpus: every case whose meta says `face: write` is
// a JSON body posted through the document's own rules tag. It compares the same three things the
// reference's writeDocument promises — a fresh extraction of the written bytes, whether anything
// changed, and, for a refusal, the type, message and structured details.
//
// The byte contract (.after.html) is phase 2: this step renders the whole document rather than
// splicing, so only the extraction is compared here.
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

// checkWriteResult compares the written document by extracting it again: the output is reparsed and
// put through the same rules tag, which is what a reader of the file would see.
func checkWriteResult(t *testing.T, name, token string, result *WriteResult) {
	t.Helper()

	rawWrite, ok := readCaseFile(t, name, ".write.json")
	if !ok {
		t.Fatalf("%s has no .write.json", name)
	}
	var want struct {
		Changed bool `json:"changed"`
	}
	if err := json.Unmarshal([]byte(rawWrite), &want); err != nil {
		t.Fatalf("%s.write.json: %v", name, err)
	}
	if result.Changed != want.Changed {
		t.Errorf("changed = %v, want %v", result.Changed, want.Changed)
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

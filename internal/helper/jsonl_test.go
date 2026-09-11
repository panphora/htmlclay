package helper

import (
	"bufio"
	"bytes"
	"strings"
	"testing"
)

// These pin the parser the shared runner actually calls. An equivalent table
// once lived beside a second copy of this parser in package main; that copy
// became unreachable when execution moved here, so the grammar was covered on
// code that no longer ran.
func TestReadRecordGrammar(t *testing.T) {
	invalidUTF8 := append([]byte(`{"type":"status","text":"`), 0xff)
	invalidUTF8 = append(invalidUTF8, []byte(`"}`)...)
	tests := []struct {
		name      string
		input     []byte
		wantType  string
		wantValue string
		wantError string
	}{
		{name: "duplicate key", input: []byte("{\"type\":\"result\",\"type\":\"status\",\"value\":null}\n"), wantError: "duplicate top-level key"},
		{name: "invalid UTF-8", input: append(invalidUTF8, '\n'), wantError: "valid UTF-8"},
		{name: "missing result value", input: []byte("{\"type\":\"result\"}\n"), wantError: "result.value is required"},
		{name: "explicit null result", input: []byte("{\"type\":\"result\",\"value\":null}\n"), wantType: "result", wantValue: "null"},
		{name: "CRLF", input: []byte("{\"type\":\"result\",\"value\":false}\r\n"), wantType: "result", wantValue: "false"},
		{name: "final record without newline", input: []byte("{\"type\":\"result\",\"value\":0}"), wantType: "result", wantValue: "0"},
		{name: "blank line", input: []byte("\n"), wantError: "blank protocol record"},
		{name: "pretty printed record", input: []byte("{\n  \"type\": \"result\",\n  \"value\": null\n}\n"), wantError: "invalid JSON record"},
		{name: "non-object", input: []byte("[]\n"), wantError: "must be a JSON object"},
		{name: "unknown type", input: []byte("{\"type\":\"partial\"}\n"), wantError: "unknown record type"},
		{name: "unknown field", input: []byte("{\"type\":\"result\",\"value\":[],\"future\":true}\n"), wantType: "result", wantValue: "[]"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec, err := readRecord(bufio.NewReaderSize(bytes.NewReader(tt.input), 8), MaxRecord)
			if tt.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantError) {
					t.Fatalf("readRecord error = %v, want text %q", err, tt.wantError)
				}
				return
			}
			if err != nil {
				t.Fatalf("readRecord error = %v, want none", err)
			}
			if rec.typeName != tt.wantType {
				t.Errorf("type = %q, want %q", rec.typeName, tt.wantType)
			}
			if string(rec.value) != tt.wantValue {
				t.Errorf("value = %q, want %q", string(rec.value), tt.wantValue)
			}
		})
	}
}

func TestReadRecordRefusesAnOversizeRecordInsteadOfTruncating(t *testing.T) {
	// A truncated record that still parses is the exact failure this replaces:
	// the old line reader discarded excess bytes and reported no overflow.
	big := append([]byte(`{"type":"result","value":"`), bytes.Repeat([]byte("a"), 4096)...)
	big = append(big, []byte("\"}\n")...)
	if _, err := readRecord(bufio.NewReader(bytes.NewReader(big)), 512); err == nil {
		t.Fatal("readRecord accepted a record past its limit, want an overflow error")
	}
}

func TestReadRecordValidatesRecordFields(t *testing.T) {
	tests := []struct {
		name      string
		input     string
		wantError string
	}{
		{name: "null type", input: `{"type":null}`, wantError: "type must be a string"},
		{name: "status text required", input: `{"type":"status"}`, wantError: "text is required"},
		{name: "status text string", input: `{"type":"status","text":null}`, wantError: "text must be a string"},
		{name: "progress object", input: `{"type":"status","text":"x","progress":null}`, wantError: "must be an object"},
		{name: "progress completed required", input: `{"type":"status","text":"x","progress":{}}`, wantError: "completed is required"},
		{name: "progress completed nonnegative", input: `{"type":"status","text":"x","progress":{"completed":-1}}`, wantError: "nonnegative finite"},
		{name: "progress total finite", input: `{"type":"status","text":"x","progress":{"completed":1,"total":1e999}}`, wantError: "nonnegative finite"},
		{name: "progress unit string", input: `{"type":"status","text":"x","progress":{"completed":1,"unit":null}}`, wantError: "unit must be a string"},
		{name: "error code required", input: `{"type":"error","message":"x"}`, wantError: "code is required"},
		{name: "error code nonempty", input: `{"type":"error","code":"","message":"x"}`, wantError: "code must not be empty"},
		{name: "error message required", input: `{"type":"error","code":"bad"}`, wantError: "message is required"},
		{name: "error message nonempty", input: `{"type":"error","code":"bad","message":""}`, wantError: "message must not be empty"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := readRecord(bufio.NewReader(strings.NewReader(tt.input)), MaxRecord)
			if err == nil || !strings.Contains(err.Error(), tt.wantError) {
				t.Fatalf("readRecord error = %v, want text %q", err, tt.wantError)
			}
		})
	}

	valid := `{"type":"status","text":"Scanning","progress":{"completed":40,"total":null,"unit":"files"}}`
	rec, err := readRecord(bufio.NewReader(strings.NewReader(valid)), MaxRecord)
	if err != nil {
		t.Fatal(err)
	}
	if rec.typeName != "status" || rec.text != "Scanning" || len(rec.progress) == 0 {
		t.Fatalf("record = %+v, want validated progress status", rec)
	}
}

func TestReadRecordLimits(t *testing.T) {
	statusPrefix := `{"type":"status","text":"ok","padding":"`
	statusSuffix := `"}`
	tests := []struct {
		name      string
		input     string
		wantError string
	}{
		{name: "status record", input: statusPrefix + strings.Repeat("x", MaxStatusRecord+1-len(statusPrefix)-len(statusSuffix)) + statusSuffix, wantError: "status record"},
		{name: "status text", input: `{"type":"status","text":"` + strings.Repeat("x", maxStatusText+1) + `"}`, wantError: "status.text"},
		{name: "error message", input: `{"type":"error","code":"bad","message":"` + strings.Repeat("x", maxStatusText+1) + `"}`, wantError: "error.message"},
		{name: "error code", input: `{"type":"error","code":"` + strings.Repeat("x", maxErrorCode+1) + `","message":"bad"}`, wantError: "error.code"},
		{name: "progress unit", input: `{"type":"status","text":"x","progress":{"completed":1,"unit":"` + strings.Repeat("x", maxProgressUnit+1) + `"}}`, wantError: "progress.unit"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := readRecord(bufio.NewReader(strings.NewReader(tt.input)), MaxRecord)
			if err == nil || !strings.Contains(err.Error(), tt.wantError) {
				t.Fatalf("readRecord error = %v, want text %q", err, tt.wantError)
			}
		})
	}

	terminalPrefix := `{"type":"result","value":"`
	terminalSuffix := `"}`
	exact := terminalPrefix + strings.Repeat("x", MaxTerminalRecord-len(terminalPrefix)-len(terminalSuffix)) + terminalSuffix
	rec, err := readRecord(bufio.NewReaderSize(strings.NewReader(exact), 4<<10), MaxRecord)
	if err != nil {
		t.Fatal(err)
	}
	if rec.typeName != "result" {
		t.Fatalf("a record at exactly the terminal limit was rejected, type = %q", rec.typeName)
	}
}

// json.Unmarshal into a json.Number accepts a JSON string holding digits, so a
// quoted progress number satisfied a grammar that asks for a number and reached
// the client unchanged. Third instance of the same family in this parser, after
// null for a string target and duplicate keys.
func TestReadRecordRefusesQuotedProgressNumbers(t *testing.T) {
	cases := []struct {
		line string
		why  string
	}{
		{`{"type":"status","text":"tick","progress":{"completed":"4"}}`, "quoted completed"},
		{`{"type":"status","text":"tick","progress":{"completed":4,"total":"8"}}`, "quoted total"},
	}
	for _, tc := range cases {
		if _, err := readRecord(bufio.NewReader(strings.NewReader(tc.line+"\n")), MaxRecord); err == nil {
			t.Errorf("%s was accepted; progress numbers must be JSON numbers", tc.why)
		}
	}
	ok := `{"type":"status","text":"tick","progress":{"completed":4,"total":8}}`
	if _, err := readRecord(bufio.NewReader(strings.NewReader(ok+"\n")), MaxRecord); err != nil {
		t.Fatalf("a genuinely numeric progress was rejected: %v", err)
	}
}

//go:build windows

package platform

import "testing"

func TestPromptNameResultSeparatesEmptyInputFromCancel(t *testing.T) {
	for _, tc := range []struct {
		out   string
		value string
		ok    bool
	}{
		{"", "", false},
		{"OK:\r\n", "", true},
		{"OK:search\r\n", "search", true},
		{"OK:search  \r\n", "search  ", true},
	} {
		value, ok, err := promptNameResult(tc.out)
		if err != nil || value != tc.value || ok != tc.ok {
			t.Errorf("promptNameResult(%q) = (%q, %v, %v), want (%q, %v, nil)", tc.out, value, ok, err, tc.value, tc.ok)
		}
	}

	if value, ok, err := promptNameResult("unexpected\r\n"); err == nil || ok || value != "" {
		t.Fatalf("unexpected output must fail closed, got (%q, %v, %v)", value, ok, err)
	}
}

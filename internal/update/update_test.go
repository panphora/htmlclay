package update

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestCheckUpdateAvailable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]string{
			"latest": "2.0",
			"url":    "https://malleable.app/download",
		})
	}))
	defer srv.Close()

	info := Check("1.0", srv.URL)
	if info == nil {
		t.Fatal("expected update info")
	}
	if info.Version != "2.0" {
		t.Errorf("expected version 2.0, got %q", info.Version)
	}
}

func TestCheckNoUpdate(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]string{
			"latest": "1.0",
			"url":    "https://malleable.app/download",
		})
	}))
	defer srv.Close()

	info := Check("1.0", srv.URL)
	if info != nil {
		t.Error("expected nil for same version")
	}
}

func TestCheckServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
	}))
	defer srv.Close()

	info := Check("1.0", srv.URL)
	if info != nil {
		t.Error("expected nil for server error")
	}
}

func TestCheckUnreachable(t *testing.T) {
	info := Check("1.0", "http://127.0.0.1:1")
	if info != nil {
		t.Error("expected nil for unreachable server")
	}
}

func TestCompareVersions(t *testing.T) {
	tests := []struct {
		a, b string
		want int
	}{
		{"1.0", "1.0", 0},
		{"2.0", "1.0", 1},
		{"1.0", "2.0", -1},
		{"1.10", "1.9", 1},
		{"10.0", "2.0", 1},
		{"1.0.1", "1.0.0", 1},
		{"1.0.0", "1.0.1", -1},
		{"1.0", "1.0.0", 0},
		{"2.0.0", "1.99.99", 1},
	}

	for _, tt := range tests {
		got := compareVersions(tt.a, tt.b)
		if got != tt.want {
			t.Errorf("compareVersions(%q, %q) = %d, want %d", tt.a, tt.b, got, tt.want)
		}
	}
}

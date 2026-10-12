package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// serveHostForTest drives the one handler directly, with no listener: the routing
// decisions are all in the handler, and the Host header is the whole of its input.
func serveHostForTest(t *testing.T, a *app, host string, mutate func(*http.Request)) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("GET", "http://"+host+"/", nil)
	if mutate != nil {
		mutate(req)
	}
	rec := httptest.NewRecorder()
	a.serveHost(rec, req)
	return rec
}

func TestServeHostBareLoopbackIsTheIndexPage(t *testing.T) {
	a := buildTestApp(t, t.TempDir(), t.TempDir())
	a.rt.port = 24190
	for _, host := range []string{"localhost:24190", "127.0.0.1:24190", "[::1]:24190"} {
		rec := serveHostForTest(t, a, host, nil)
		if rec.Code != http.StatusOK {
			t.Errorf("%s: status = %d, want 200", host, rec.Code)
		}
		if !strings.Contains(rec.Body.String(), "HTML Clay is running") {
			t.Errorf("%s: body = %q, want the index page", host, rec.Body.String())
		}
	}
}

func TestServeHostRefusesForeignAndWrongPort(t *testing.T) {
	a := buildTestApp(t, t.TempDir(), t.TempDir())
	a.rt.port = 24190
	for _, host := range []string{"evil.example:24190", "x-0000.localhost:24191", "Bad_Label.localhost:24190"} {
		if rec := serveHostForTest(t, a, host, nil); rec.Code != http.StatusForbidden {
			t.Errorf("%s: status = %d, want 403", host, rec.Code)
		}
	}
}

func TestServeHostUnknownSlugIsTheRecoveryPage(t *testing.T) {
	a := buildTestApp(t, t.TempDir(), t.TempDir())
	a.rt.port = 24190
	rec := serveHostForTest(t, a, "nobody-0000.localhost:24190", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	if body := rec.Body.String(); body != string(recoveryPage) {
		t.Fatalf("body = %q, want the recovery page", body)
	}
	if got := rec.Header().Get("Referrer-Policy"); got != "no-referrer" {
		t.Fatalf("Referrer-Policy = %q, want no-referrer", got)
	}
}

func TestServeHostCrossSiteFetchIsRefusedEvenForTheIndexPage(t *testing.T) {
	a := buildTestApp(t, t.TempDir(), t.TempDir())
	a.rt.port = 24190
	rec := serveHostForTest(t, a, "localhost:24190", func(r *http.Request) {
		r.Header.Set("Sec-Fetch-Site", "cross-site")
		r.Header.Set("Sec-Fetch-Mode", "cors")
	})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
}

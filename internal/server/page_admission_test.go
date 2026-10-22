package server

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/panphora/htmlclay/internal/pageadmission"
	"github.com/panphora/htmlclay/internal/session"
	"github.com/panphora/htmlclay/internal/specwire"
)

const pageHybrid = `<script type="application/json" merge="page" clay="live">{"blocks":{}}</script>`
const pageYJS = `<script type="application/json" merge="page" clay="live">{"v":1,"y":{"seed":"opaque"}}</script>`

func pageDocument(store string) string {
	return `<!DOCTYPE html><html><body><p>outside</p>` + store + `</body></html>`
}

func savePageThroughMux(t *testing.T, srv *Server, f *session.File, body, condition, conversion, saveID string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("POST", "/_/save/"+f.Token, strings.NewReader(body))
	req.Host = fmt.Sprintf("127.0.0.1:%d", srv.port)
	sameOriginHeaders(req)
	if condition != "" {
		req.Header.Set("If-Match", condition)
	}
	if conversion != "" {
		req.Header.Set("Page-Conversion", conversion)
	}
	if saveID != "" {
		req.Header.Set("Save-ID", saveID)
	}
	w := httptest.NewRecorder()
	srv.httpServer.Handler.ServeHTTP(w, req)
	return w
}

func pageBase(t *testing.T, source string) string {
	t.Helper()
	base, err := pageadmission.Baseline(pageadmission.Inspect(source).Tags)
	if err != nil {
		t.Fatal(err)
	}
	return base
}

func TestPageAdmissionOrdinarySavePreservesBytesAndStamp(t *testing.T) {
	srv, _, _ := setupHandlerTest(t)
	old := pageDocument("")
	f := registerPageWithContent(t, srv, "ordinary-page.htmlclay", old)
	next := strings.Replace(old, "outside", "changed", 1)
	w := savePageThroughMux(t, srv, f, next, specwire.Etag([]byte(old)), "", "")
	if w.Code != http.StatusOK || docString(t, f.AbsPath) != next || decode(t, w)["etag"] != specwire.Etag([]byte(next)) {
		t.Fatalf("ordinary document saves unchanged: %d, %s", w.Code, w.Body.String())
	}
}

func TestPageAdmissionConvertedSaveRefusalsLeaveNoTrace(t *testing.T) {
	srv, _, _ := setupHandlerTest(t)
	converted := pageDocument(pageYJS)
	f := registerPageWithContent(t, srv, "converted-page.htmlclay", converted)
	f.Lock()
	f.RecordSaveReceipt("earlier", specwire.Etag([]byte(converted)))
	f.Unlock()
	before := listVersionNames(t, srv, f)
	for _, test := range []struct{ name, next string }{
		{"hybrid capture", pageDocument(pageHybrid)},
		{"mixed capture", pageDocument(pageYJS + pageHybrid)},
	} {
		t.Run(test.name, func(t *testing.T) {
			w := savePageThroughMux(t, srv, f, test.next, specwire.Etag([]byte(converted)), "", "new")
			body := decode(t, w)
			if w.Code != http.StatusConflict || body["code"] != "page-format" || body["reason"] != nil {
				t.Fatalf("refusal = %d %s", w.Code, w.Body.String())
			}
			if on := docString(t, f.AbsPath); on != converted {
				t.Fatalf("refusal changed disk: %q", on)
			}
			if after := listVersionNames(t, srv, f); len(after) != len(before) {
				t.Fatalf("refusal recorded a version: %v to %v", before, after)
			}
			f.Lock()
			receipt := f.SaveReceiptFor(specwire.Etag([]byte(converted)))
			f.Unlock()
			if receipt != "earlier" {
				t.Fatalf("refusal replaced receipt with %q", receipt)
			}
		})
	}
}

func TestPageAdmissionSaveStripsPageTag(t *testing.T) {
	srv, _, _ := setupHandlerTest(t)
	tools := `<script type="application/json" merge="page.tools" clay="live">{}</script>`
	document := func(lines ...string) string {
		return `<!DOCTYPE html><html><head>` + "\n" + strings.Join(lines, "\n") +
			"\n" + `</head><body><article class="page" data-page="1"><h1 data-id="title"></h1></article></body></html>`
	}
	want := document(tools)
	f := registerPageWithContent(t, srv, "stripped-page.htmlclay", want)
	w := savePageThroughMux(t, srv, f, document(tools, pageYJS), "", "", "")
	if w.Code != http.StatusOK {
		t.Fatalf("page save = %d %s", w.Code, w.Body.String())
	}
	on := docString(t, f.AbsPath)
	if on != want || pageadmission.Inspect(on).Kind != "none" ||
		!strings.Contains(on, `merge="page.tools"`) || !strings.Contains(on, `data-page="1"`) {
		t.Fatalf("stored bytes = %q", on)
	}
	if got := decode(t, w)["etag"]; got != specwire.Etag([]byte(want)) {
		t.Fatalf("etag = %v, want the stripped stamp", got)
	}
}

func TestPageAdmissionOutsideEditWithoutPageTag(t *testing.T) {
	srv, _, _ := setupHandlerTest(t)
	converted := pageDocument(pageYJS)
	f := registerPageWithContent(t, srv, "outside-page.htmlclay", converted)
	tagless := pageDocument("")
	w := savePageThroughMux(t, srv, f, tagless, "", "", "")
	if w.Code != http.StatusOK || docString(t, f.AbsPath) != tagless || decode(t, w)["etag"] != specwire.Etag([]byte(tagless)) {
		t.Fatalf("a save without the page tag is an outside edit: %d %s", w.Code, w.Body.String())
	}
}

func TestPageAdmissionConversionAndConditions(t *testing.T) {
	srv, _, _ := setupHandlerTest(t)
	hybrid := pageDocument(pageHybrid)
	converted := pageDocument(pageYJS)
	f := registerPageWithContent(t, srv, "hybrid-page.htmlclay", hybrid)
	base := pageBase(t, hybrid)
	header := "v1; source=file; base=" + base
	stamp := specwire.Etag([]byte(hybrid))
	for _, test := range []struct {
		name, condition, conversion, code, reason string
		status                                    int
	}{
		{"implicit conversion", stamp, "", "page-conversion-required", "implicit-conversion", 409},
		{"stale base", stamp, "v1; source=file; base=" + strings.Repeat("0", 64), "page-conversion-stale", "source-changed", 409},
		{"room source", stamp, "v1; source=room; base=" + base + "; epoch=e1", "page-conversion-refused", "invalid-header", 400},
		{"missing condition", "", header, "page-conversion-stale", "file-changed", 428},
		{"wildcard condition", "*", header, "page-conversion-stale", "file-changed", 428},
		{"list condition", stamp + ", " + stamp, header, "page-conversion-stale", "file-changed", 428},
		{"stale If-Match before page decision", "stale", "", "conflict", "", 412},
	} {
		t.Run(test.name, func(t *testing.T) {
			before := len(listVersionNames(t, srv, f))
			w := savePageThroughMux(t, srv, f, converted, test.condition, test.conversion, "")
			body := decode(t, w)
			if w.Code != test.status || body["code"] != test.code || body["reason"] != test.reason && test.reason != "" {
				t.Fatalf("refusal = %d %s, want %d %s/%s", w.Code, w.Body.String(), test.status, test.code, test.reason)
			}
			if docString(t, f.AbsPath) != hybrid || len(listVersionNames(t, srv, f)) != before {
				t.Fatal("refusal changed disk or versions")
			}
		})
	}
	w := savePageThroughMux(t, srv, f, converted, stamp, header, "")
	if w.Code != http.StatusOK || docString(t, f.AbsPath) != converted {
		t.Fatalf("valid file conversion = %d %s", w.Code, w.Body.String())
	}
}

func TestPageAdmissionInitialCaptureNeedsExactStamp(t *testing.T) {
	srv, _, _ := setupHandlerTest(t)
	empty := pageDocument("")
	f := registerPageWithContent(t, srv, "initial-page.htmlclay", empty)
	converted := pageDocument(pageYJS)
	w := savePageThroughMux(t, srv, f, converted, "", "", "")
	if w.Code != 428 || decode(t, w)["reason"] != "file-changed" {
		t.Fatalf("missing condition = %d %s", w.Code, w.Body.String())
	}
	w = savePageThroughMux(t, srv, f, converted, specwire.Etag([]byte(empty)), "", "")
	if w.Code != 200 || docString(t, f.AbsPath) != converted {
		t.Fatalf("initial capture = %d %s", w.Code, w.Body.String())
	}
}

func TestPageAdmissionInitialAndConversionAcceptNormalizedSingleStamps(t *testing.T) {
	for _, test := range []struct {
		name, source string
		convert      bool
	}{
		{"initial", pageDocument(""), false},
		{"conversion", pageDocument(pageHybrid), true},
	} {
		for _, form := range []string{"bare", "quoted", "weak"} {
			t.Run(test.name+"/"+form, func(t *testing.T) {
				srv, _, _ := setupHandlerTest(t)
				f := registerPageWithContent(t, srv, "normalized-page.htmlclay", test.source)
				stamp := specwire.Etag([]byte(test.source))
				condition := stamp
				switch form {
				case "quoted":
					condition = `"` + stamp + `"`
				case "weak":
					condition = `W/"` + stamp + `"`
				}
				header := ""
				if test.convert {
					header = "v1; source=file; base=" + pageBase(t, test.source)
				}
				w := savePageThroughMux(t, srv, f, pageDocument(pageYJS), condition, header, "")
				if w.Code != 200 {
					t.Fatalf("single stamp = %d %s", w.Code, w.Body.String())
				}
			})
		}
	}
}

func TestPageAdmissionExplicitEmptyHeaderAndChangedFile(t *testing.T) {
	srv, _, _ := setupHandlerTest(t)
	source := pageDocument(pageHybrid)
	f := registerPageWithContent(t, srv, "header-page.htmlclay", source)
	base := pageBase(t, source)
	req := httptest.NewRequest("POST", "/_/save/"+f.Token, strings.NewReader(pageDocument(pageYJS)))
	req.Host = fmt.Sprintf("127.0.0.1:%d", srv.port)
	sameOriginHeaders(req)
	req.Header.Set("If-Match", specwire.Etag([]byte(source)))
	req.Header.Set("Page-Conversion", "")
	w := httptest.NewRecorder()
	srv.httpServer.Handler.ServeHTTP(w, req)
	if w.Code != 400 || decode(t, w)["reason"] != "invalid-header" {
		t.Fatalf("explicit metadata remains mandatory validation: %d %s", w.Code, w.Body.String())
	}
	changed := strings.Replace(source, "outside", "new outside text", 1)
	if err := os.WriteFile(f.AbsPath, []byte(changed), 0644); err != nil {
		t.Fatal(err)
	}
	condition := specwire.Etag([]byte(source))
	header := "v1; source=file; base=" + base
	w = savePageThroughMux(t, srv, f, pageDocument(pageYJS), condition, header, "")
	if w.Code != 412 || docString(t, f.AbsPath) != changed {
		t.Fatalf("surrounding HTML is bound by If-Match: %d %s", w.Code, w.Body.String())
	}
	w = savePageThroughMux(t, srv, f, pageDocument(pageYJS), specwire.Etag([]byte(changed)), header, "")
	if w.Code != 200 {
		t.Fatalf("same page baseline with current file stamp = %d %s", w.Code, w.Body.String())
	}
}

func TestPageAdmissionDataAPI(t *testing.T) {
	srv, _, _ := setupHandlerTest(t)
	preserving := `<!DOCTYPE html><html><head><script type="application/json" data-rules-name="api" data-rules-version="1">{"title":"h1"}</script></head><body><h1>Old</h1>` + pageYJS + `</body></html>`
	f := registerPageWithContent(t, srv, "data-page.htmlclay", preserving)
	w := postDataWrite(t, srv, "/_/api/data-page.htmlclay", `{"title":"New"}`, nil)
	if w.Code != 200 || !strings.Contains(docString(t, f.AbsPath), `<h1>New</h1>`) || pageadmission.Inspect(docString(t, f.AbsPath)).Kind != "yjs" {
		t.Fatalf("data API preserving write = %d %s", w.Code, w.Body.String())
	}
	breaking := `<!DOCTYPE html><html><head><script type="application/json" data-rules-name="api" data-rules-version="1">{"body":"body@textContent"}</script></head><body><p>Old</p>` + pageYJS + `</body></html>`
	g := registerPageWithContent(t, srv, "data-break-page.htmlclay", breaking)
	w = postDataWrite(t, srv, "/_/api/data-break-page.htmlclay", `{"body":"replacement"}`, nil)
	if w.Code != 200 {
		t.Fatalf("data API outside edit = %d %s", w.Code, w.Body.String())
	}
	if written := docString(t, g.AbsPath); !strings.Contains(written, "replacement") || pageadmission.Inspect(written).Kind != "none" {
		t.Fatalf("data API outside edit wrote %q", written)
	}
}

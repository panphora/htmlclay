package server

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"

	"golang.org/x/net/html"

	"github.com/panphora/htmlclay/internal/clayruntime"
	"github.com/panphora/htmlclay/internal/session"
)

// journalFixtures are the three real Journals a person opens in HTML Clay: the two the old
// site shipped and the one the template hands out today.
var journalFixtures = []struct {
	label string
	file  string
}{
	{"1.0.0", "journal-1.0.0.html"},
	{"1.1.0", "journal-1.1.0.html"},
	{"current", "journal-current.html"},
}

// journalPreconnects are the only absolute addresses a served Journal may keep: hints that cost
// nothing offline, and the two an old Journal writes for the Google Fonts faces.
var journalPreconnects = []string{"https://fonts.googleapis.com", "https://fonts.gstatic.com"}

// journalOfflineTransport turns every request that leaves the process into a test failure, so a
// runtime the release does not carry cannot be papered over by a working network.
type journalOfflineTransport struct {
	t *testing.T
}

func (transport journalOfflineTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	transport.t.Errorf("a request left the process: %s %s", request.Method, request.URL)
	return nil, fmt.Errorf("offline: %s", request.URL)
}

func blockJournalNetwork(t *testing.T) {
	t.Helper()
	previous := http.DefaultTransport
	http.DefaultTransport = journalOfflineTransport{t: t}
	t.Cleanup(func() { http.DefaultTransport = previous })
}

func readJournalFixture(t *testing.T, file string) string {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("testdata", "journal", file))
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

// openJournal copies a fixture into the server's home folder, opens it the way the app does and
// returns the served bytes.
func openJournal(t *testing.T, srv *Server, file string) (*session.File, string, string) {
	t.Helper()
	page := writeHome(t, srv, file, readJournalFixture(t, file))
	f, err := srv.sessions.Register(page, session.ViaOsOpen)
	if err != nil {
		t.Fatal(err)
	}
	w := serve(t, srv, "/"+file, nil)
	if w.Code != 200 {
		t.Fatalf("serve %s: %d %s", file, w.Code, w.Body.String())
	}
	return f, page, w.Body.String()
}

type journalAsset struct {
	rel   string
	value string
}

// journalAssets lists every src and href a browser would load from a script or a link element.
func journalAssets(t *testing.T, document string) []journalAsset {
	t.Helper()
	assets := []journalAsset{}
	tokenizer := html.NewTokenizer(strings.NewReader(document))
	for {
		switch tokenizer.Next() {
		case html.ErrorToken:
			if err := tokenizer.Err(); err != io.EOF {
				t.Fatalf("the document could not be tokenized: %v", err)
			}
			return assets
		case html.StartTagToken, html.SelfClosingTagToken:
			token := tokenizer.Token()
			if token.Data != "script" && token.Data != "link" {
				continue
			}
			asset := journalAsset{}
			for _, attr := range token.Attr {
				switch attr.Key {
				case "src":
					asset.value = attr.Val
				case "href":
					asset.value = attr.Val
				case "rel":
					asset.rel = attr.Val
				}
			}
			if asset.value != "" {
				assets = append(assets, asset)
			}
		}
	}
}

// journalURLs sorts a served document's script and link addresses into the paths the release now
// answers, the runtime URLs the projection left public, and everything absolute left outside it.
func journalURLs(t *testing.T, document string) (projected, runtime, external []string) {
	t.Helper()
	aliases := clayruntime.Releases[clayruntime.Selected].Aliases
	for _, asset := range journalAssets(t, document) {
		if strings.HasPrefix(asset.value, clayruntime.LocalPrefix) {
			projected = append(projected, asset.value)
			continue
		}
		if strings.HasPrefix(asset.value, "https://clayjs.com/") {
			runtime = append(runtime, asset.value)
			continue
		}
		if _, ok := aliases[asset.value]; ok {
			runtime = append(runtime, asset.value)
			continue
		}
		if parsed, err := url.Parse(asset.value); err == nil && (parsed.Scheme == "http" || parsed.Scheme == "https") {
			external = append(external, asset.value)
		}
	}
	sort.Strings(projected)
	sort.Strings(runtime)
	sort.Strings(external)
	return projected, runtime, external
}

// journalReleaseName is the release path a projected local URL names.
func journalReleaseName(t *testing.T, local string) string {
	t.Helper()
	parsed, err := url.Parse(local)
	if err != nil {
		t.Fatalf("%s is not a URL: %v", local, err)
	}
	prefix := clayruntime.LocalPrefix + clayruntime.Selected + "/"
	if !strings.HasPrefix(parsed.Path, prefix) {
		t.Fatalf("%s does not name the selected release %s", local, clayruntime.Selected)
	}
	return strings.TrimPrefix(parsed.Path, prefix)
}

var (
	journalTagPattern  = regexp.MustCompile(`(?is)<(?:script|link)\b[^>]*>`)
	journalAttrPattern = regexp.MustCompile(`(?i)(?:src|href)\s*=\s*(?:"([^"]*)"|'([^']*)')`)
)

// journalRawRuntimeURLs reads the runtime addresses of a document as the bytes spell them, so a
// saved file can be compared against its original spelling and not just its decoded value.
func journalRawRuntimeURLs(t *testing.T, document string) []string {
	t.Helper()
	aliases := clayruntime.Releases[clayruntime.Selected].Aliases
	values := []string{}
	for _, tag := range journalTagPattern.FindAllString(document, -1) {
		for _, match := range journalAttrPattern.FindAllStringSubmatch(tag, -1) {
			raw := match[1]
			if raw == "" {
				raw = match[2]
			}
			value := html.UnescapeString(raw)
			if strings.HasPrefix(value, "https://clayjs.com/") {
				values = append(values, raw)
				continue
			}
			if _, ok := aliases[value]; ok {
				values = append(values, raw)
			}
		}
	}
	return values
}

// A served Journal names no public runtime URL any more: every one of them, the classic CDN
// libraries and the Google Fonts stylesheet included, is a path this host serves. What is left
// absolute is logged, so each copy's outside addresses are on the record.
func TestJournalOfflineServedProjection(t *testing.T) {
	blockJournalNetwork(t)
	srv, _, _ := setupHandlerTest(t)

	for _, fixture := range journalFixtures {
		t.Run(fixture.label, func(t *testing.T) {
			_, _, served := openJournal(t, srv, fixture.file)
			projected, runtime, external := journalURLs(t, served)
			if len(projected) == 0 {
				t.Fatalf("the served %s names no local runtime URL:\n%s", fixture.file, served)
			}
			if len(runtime) > 0 {
				t.Errorf("the served %s still names public runtime URLs: %v", fixture.file, runtime)
			}
			if len(external) > 0 && !slices.Equal(external, journalPreconnects) {
				t.Errorf("the served %s keeps external URLs %v, want only the preconnect hints %v",
					fixture.file, external, journalPreconnects)
			}
			for _, asset := range journalAssets(t, served) {
				if !slices.Contains(external, asset.value) {
					continue
				}
				if !strings.Contains(strings.ToLower(asset.rel), "preconnect") {
					t.Errorf("the served %s loads %s from outside the release", fixture.file, asset.value)
				}
			}
			t.Logf("%s external URLs: %v", fixture.file, external)
		})
	}
}

// Every URL a served Journal asks for is answered from the embedded release, byte for byte.
func TestJournalOfflineProjectedURLsServeReleaseBytes(t *testing.T) {
	blockJournalNetwork(t)
	srv, _, _ := setupHandlerTest(t)
	release := clayruntime.Releases[clayruntime.Selected]

	for _, fixture := range journalFixtures {
		t.Run(fixture.label, func(t *testing.T) {
			_, _, served := openJournal(t, srv, fixture.file)
			projected, _, _ := journalURLs(t, served)
			if len(projected) == 0 {
				t.Fatalf("the served %s names no local runtime URL", fixture.file)
			}
			for _, local := range projected {
				name := journalReleaseName(t, local)
				entry, ok := release.Files[name]
				if !ok {
					t.Errorf("%s is not a file of release %s", name, clayruntime.Selected)
					continue
				}
				w := runtimeRequest(t, srv, "GET", local, nil)
				if w.Code != 200 {
					t.Errorf("GET %s = %d", local, w.Code)
					continue
				}
				if ct := w.Header().Get("Content-Type"); ct != entry.Type {
					t.Errorf("%s Content-Type = %q, want %q", local, ct, entry.Type)
				}
				sum := sha256.Sum256(w.Body.Bytes())
				if got := hex.EncodeToString(sum[:]); got != entry.SHA256 {
					t.Errorf("%s bytes hash %s, want the index's %s", local, got, entry.SHA256)
				}
			}
		})
	}
}

// An old Journal's OverType and highlight.js URLs resolve to the packaged compatibility builds,
// and no near miss does: a URL the release index does not name stays public.
func TestRuntimeAliasJournalClassicLibraries(t *testing.T) {
	blockJournalNetwork(t)
	srv, _, _ := setupHandlerTest(t)

	_, lockBytes, ok := clayruntime.Asset(clayruntime.Selected, "compat/journal-assets.lock.json")
	if !ok {
		t.Fatalf("release %s carries no compat/journal-assets.lock.json", clayruntime.Selected)
	}
	var lock struct {
		Aliases map[string]struct {
			Path   string `json:"path"`
			SHA256 string `json:"sha256"`
		} `json:"aliases"`
	}
	if err := json.Unmarshal(lockBytes, &lock); err != nil {
		t.Fatalf("the compat lock is not JSON: %v", err)
	}

	classic := map[string]string{
		"compat/overtype-2.6.1.min.js":   "OverType",
		"compat/highlight-11.9.0.min.js": "hljs",
	}
	for _, fixture := range journalFixtures[:2] {
		t.Run(fixture.label, func(t *testing.T) {
			_, _, served := openJournal(t, srv, fixture.file)
			checked := 0
			for alias, entry := range lock.Aliases {
				marker, wanted := classic[entry.Path]
				if !wanted {
					continue
				}
				local := clayruntime.LocalPrefix + clayruntime.Selected + "/" + entry.Path
				if !strings.Contains(served, local) {
					t.Errorf("the served %s does not load %s from %s", fixture.file, alias, local)
					continue
				}
				w := runtimeRequest(t, srv, "GET", local, nil)
				if w.Code != 200 {
					t.Errorf("GET %s = %d", local, w.Code)
					continue
				}
				sum := sha256.Sum256(w.Body.Bytes())
				if got := hex.EncodeToString(sum[:]); got != entry.SHA256 {
					t.Errorf("%s bytes hash %s, want the compat lock's %s", local, got, entry.SHA256)
				}
				if !strings.Contains(w.Body.String(), marker) {
					t.Errorf("%s does not look like %s", local, marker)
				}
				checked++
			}
			if checked != len(classic) {
				t.Fatalf("checked %d classic libraries, want %d", checked, len(classic))
			}
		})
	}

	nearMisses := []struct {
		label string
		url   string
	}{
		{"unknown version", "https://cdn.jsdelivr.net/npm/overtype@2.6.2/dist/overtype.min.js"},
		{"query string", "https://cdn.jsdelivr.net/npm/overtype@2.6.1/dist/overtype.min.js?v=1"},
		{"another host", "https://unpkg.com/overtype@2.6.1/dist/overtype.min.js"},
	}
	for _, nearMiss := range nearMisses {
		t.Run("no alias "+nearMiss.label, func(t *testing.T) {
			document := `<!DOCTYPE html>` + "\n" + `<html lang="en">` + "\n" + `<head>` + "\n" +
				`<script src="https://clayjs.com/v1/clay.js"></script>` + "\n" +
				`<script src="` + nearMiss.url + `"></script>` + "\n" +
				`</head>` + "\n" + `<body><p>journal</p></body>` + "\n" + `</html>`
			page := writeHome(t, srv, "nearmiss.htmlclay", document)
			if _, err := srv.sessions.Register(page, session.ViaOsOpen); err != nil {
				t.Fatal(err)
			}
			w := serve(t, srv, "/nearmiss.htmlclay", nil)
			if w.Code != 200 {
				t.Fatalf("serve: %d %s", w.Code, w.Body.String())
			}
			served := w.Body.String()
			if !strings.Contains(served, `src="`+nearMiss.url+`"`) {
				t.Errorf("%s lost its own URL %s:\n%s", nearMiss.label, nearMiss.url, served)
			}
			if !strings.Contains(served, clayruntime.LocalPrefix+clayruntime.Selected+"/clay.js") {
				t.Errorf("a document naming %s was not projected at all:\n%s", nearMiss.label, served)
			}
		})
	}
}

var journalCSSURLPattern = regexp.MustCompile(`url\(\s*['"]?([^'")]*)['"]?\s*\)`)

// The Google Fonts stylesheet an old Journal asks for is served as CSS whose every font resolves,
// relative to the stylesheet's own path, to a release file with the release's font type.
func TestJournalOfflineFontStylesheetAlias(t *testing.T) {
	blockJournalNetwork(t)
	srv, _, _ := setupHandlerTest(t)
	release := clayruntime.Releases[clayruntime.Selected]

	for _, fixture := range journalFixtures {
		t.Run(fixture.label, func(t *testing.T) {
			_, _, served := openJournal(t, srv, fixture.file)
			projected, _, _ := journalURLs(t, served)
			source := readJournalFixture(t, fixture.file)
			for alias, target := range release.Aliases {
				if !strings.Contains(source, alias) {
					continue
				}
				local := clayruntime.LocalPrefix + clayruntime.Selected + "/" + target
				if !slices.Contains(projected, local) {
					t.Errorf("the served %s does not load the aliased %s from %s", fixture.file, alias, local)
				}
			}
			stylesheets := 0
			fonts := 0
			for _, local := range projected {
				name := journalReleaseName(t, local)
				entry, ok := release.Files[name]
				if !ok || !strings.HasPrefix(entry.Type, "text/css") {
					continue
				}
				w := runtimeRequest(t, srv, "GET", local, nil)
				if w.Code != 200 {
					t.Errorf("GET %s = %d", local, w.Code)
					continue
				}
				if ct := w.Header().Get("Content-Type"); ct != entry.Type {
					t.Errorf("%s Content-Type = %q, want %q", local, ct, entry.Type)
				}
				stylesheets++
				base := &url.URL{Scheme: "http", Host: "127.0.0.1", Path: local}
				for _, match := range journalCSSURLPattern.FindAllStringSubmatch(w.Body.String(), -1) {
					reference, err := url.Parse(match[1])
					if err != nil {
						t.Errorf("%s holds an unparseable url(%s): %v", local, match[1], err)
						continue
					}
					if reference.IsAbs() {
						t.Errorf("%s holds the absolute url(%s)", local, match[1])
						continue
					}
					target := base.ResolveReference(reference)
					font := runtimeRequest(t, srv, "GET", target.RequestURI(), nil)
					if font.Code != 200 {
						t.Errorf("%s asks for %s, which answered %d", local, target.RequestURI(), font.Code)
						continue
					}
					fontEntry, ok := release.Files[strings.TrimPrefix(target.Path, clayruntime.LocalPrefix+clayruntime.Selected+"/")]
					if !ok {
						t.Errorf("%s asks for %s, which the release does not list", local, target.Path)
						continue
					}
					if got := font.Header().Get("Content-Type"); got != fontEntry.Type {
						t.Errorf("%s Content-Type = %q, want %q", target.Path, got, fontEntry.Type)
					}
					if !strings.HasPrefix(fontEntry.Type, "font/") {
						t.Errorf("%s is %q, want a font type", target.Path, fontEntry.Type)
					}
					fonts++
				}
			}
			if stylesheets == 0 {
				t.Fatalf("the served %s loads no stylesheet from the release", fixture.file)
			}
			if fonts == 0 {
				t.Fatalf("the served %s resolves no font from its stylesheet", fixture.file)
			}
		})
	}
}

// journalChunkDescriptor is the chunk descriptor a served clay.js closes with: the directory
// its chunks live in and the chunks it preloads, the loader among them.
type journalChunkDescriptor struct {
	Dir     string   `json:"dir"`
	Preload []string `json:"preload"`
}

// journalChunkPattern is the loader's asset import table: each asset ID and the chunk that
// belongs to it, quoted when the ID holds a slash and bare when it does not.
var journalChunkPattern = regexp.MustCompile(`(?:["']([a-z][a-z/-]*)["']|([a-z][a-z/-]*)):\(\)=>import\("\./([^"]+)"\)`)

// journalClayChunks reads the descriptor the browser parses to decide which chunk to boot.
func journalClayChunks(t *testing.T, clayJS string) journalChunkDescriptor {
	t.Helper()
	start := strings.LastIndex(clayJS, "({")
	if start < 0 {
		t.Fatalf("the served clay.js closes with no chunk descriptor")
	}
	end := strings.Index(clayJS[start:], "})")
	if end < 0 {
		t.Fatalf("the served clay.js chunk descriptor is not closed")
	}
	var descriptor journalChunkDescriptor
	if err := json.Unmarshal([]byte(clayJS[start+1:start+end+1]), &descriptor); err != nil {
		t.Fatalf("the served clay.js chunk descriptor is not JSON: %v", err)
	}
	return descriptor
}

// journalChunks is the loader's asset import table: the asset ID to chunk map it boots with.
func journalChunks(t *testing.T, loader string) map[string]string {
	t.Helper()
	chunks := map[string]string{}
	for _, match := range journalChunkPattern.FindAllStringSubmatch(loader, -1) {
		id := match[1]
		if id == "" {
			id = match[2]
		}
		chunks[id] = match[3]
	}
	return chunks
}

var (
	journalLangsPattern     = regexp.MustCompile(`const LANGS = \{([^}]*)\}`)
	journalLangValuePattern = regexp.MustCompile(`:\s*'([^']+)'`)
)

// journalLateLanguages is every highlight.js language the current Journal can ask for, taken
// from the template's own LANGS table so a language added there is checked here too.
func journalLateLanguages(t *testing.T, source string) []string {
	t.Helper()
	table := journalLangsPattern.FindStringSubmatch(source)
	if table == nil {
		t.Fatalf("the current Journal holds no LANGS table")
	}
	seen := map[string]bool{}
	languages := []string{}
	for _, match := range journalLangValuePattern.FindAllStringSubmatch(table[1], -1) {
		if seen[match[1]] {
			continue
		}
		seen[match[1]] = true
		languages = append(languages, match[1])
	}
	if len(languages) == 0 {
		t.Fatalf("the current Journal's LANGS table names no language")
	}
	sort.Strings(languages)
	return languages
}

// The libraries the current Journal loads only when it needs them are in the release under the
// chunk names its own loader asks for, so the first code block or markdown paste works with no
// network. The requests are the ones the served release makes, not names guessed from a source
// tree, so a chunk the binary cannot answer fails here.
func TestJournalOfflineLateLoads(t *testing.T) {
	blockJournalNetwork(t)
	srv, _, _ := setupHandlerTest(t)
	release := clayruntime.Releases[clayruntime.Selected]

	fixture := journalFixtures[2]
	source := readJournalFixture(t, fixture.file)
	_, _, served := openJournal(t, srv, fixture.file)

	clayJS := runtimeRequest(t, srv, "GET", runtimeAssetTarget("clay.js"), nil)
	if clayJS.Code != 200 {
		t.Fatalf("GET clay.js = %d", clayJS.Code)
	}
	descriptor := journalClayChunks(t, clayJS.Body.String())
	loader := ""
	for _, name := range descriptor.Preload {
		if strings.HasPrefix(name, "loader-") {
			loader = name
			break
		}
	}
	if loader == "" {
		t.Fatalf("the served clay.js boots no loader chunk: %v", descriptor.Preload)
	}
	loaderFile := descriptor.Dir + loader
	loaderResponse := runtimeRequest(t, srv, "GET", runtimeAssetTarget(loaderFile), nil)
	if loaderResponse.Code != 200 {
		t.Fatalf("GET %s = %d", loaderFile, loaderResponse.Code)
	}
	chunks := journalChunks(t, loaderResponse.Body.String())
	if len(chunks) == 0 {
		t.Fatalf("%s carries no asset import table", loaderFile)
	}

	late := []string{"overtype", "marked", "highlight"}
	for _, language := range journalLateLanguages(t, source) {
		late = append(late, "highlight/"+language)
	}
	for _, id := range late {
		chunk, ok := chunks[id]
		if !ok || chunk == "" {
			t.Errorf("%s carries no chunk for %q", loaderFile, id)
			continue
		}
		publicPath := descriptor.Dir + chunk
		entry, ok := release.Files[publicPath]
		if !ok {
			t.Errorf("%s asks for %s for %q, which is not a file of release %s", loaderFile, publicPath, id, clayruntime.Selected)
			continue
		}
		if strings.Contains(served, publicPath) {
			t.Errorf("%s is in the served document, so it is not a late load", publicPath)
		}
		w := runtimeRequest(t, srv, "GET", runtimeAssetTarget(publicPath), nil)
		if w.Code != 200 {
			t.Errorf("GET %s = %d, want 200 (%q loads it late)", publicPath, w.Code, id)
			continue
		}
		if ct := w.Header().Get("Content-Type"); ct != entry.Type {
			t.Errorf("%s Content-Type = %q, want %q", publicPath, ct, entry.Type)
		}
		sum := sha256.Sum256(w.Body.Bytes())
		if got := hex.EncodeToString(sum[:]); got != entry.SHA256 {
			t.Errorf("%s bytes hash %s, want the index's %s", publicPath, got, entry.SHA256)
		}
	}

	for _, name := range []string{"overtype", "marked", "highlight"} {
		if !strings.Contains(source, "assets.load('"+name+"')") {
			t.Errorf("the %s Journal never asks for %q", fixture.file, name)
		}
	}
	if !strings.Contains(source, "assets.load(`highlight/") {
		t.Errorf("the %s Journal never asks for a highlight language", fixture.file)
	}
}

// A Journal edited in HTML Clay saves as the file it always was: no local address, no descriptor,
// the same public runtime URLs byte for byte, every entry intact, and it opens again.
func TestJournalOfflineSaveAndReopen(t *testing.T) {
	blockJournalNetwork(t)
	srv, _, _ := setupHandlerTest(t)

	for _, fixture := range journalFixtures {
		t.Run(fixture.label, func(t *testing.T) {
			source := readJournalFixture(t, fixture.file)
			f, _, served := openJournal(t, srv, fixture.file)
			etag := decodedDocumentETag(t, []byte(served))
			if etag == "" {
				t.Fatal("the served document carries no documentetag to save against")
			}

			_, entry := journalEntryCopy(t, source)
			edited := served
			template := journalEntryTemplate(t, source)
			if !strings.Contains(edited, template) {
				t.Fatalf("the served %s lost the entry record it must copy", fixture.file)
			}
			edited = strings.Replace(edited, template, template+"\n"+entry, 1)

			w := saveThroughMux(t, srv, f, edited, etag)
			if w.Code != 200 {
				t.Fatalf("save: %d %s", w.Code, w.Body.String())
			}
			disk, err := os.ReadFile(f.AbsPath)
			if err != nil {
				t.Fatal(err)
			}
			saved := string(disk)
			for _, local := range []string{"/_/clayjs/", "clay-runtime-map"} {
				if strings.Contains(saved, local) {
					t.Errorf("the saved %s kept %q on disk", fixture.file, local)
				}
			}
			_, fixtureRuntime, _ := journalURLs(t, source)
			if len(fixtureRuntime) == 0 {
				t.Fatalf("the %s fixture names no runtime URL", fixture.file)
			}
			savedProjected, savedRuntime, _ := journalURLs(t, saved)
			if len(savedProjected) > 0 {
				t.Errorf("the saved %s still holds local runtime URLs: %v", fixture.file, savedProjected)
			}
			if !slices.Equal(fixtureRuntime, savedRuntime) {
				t.Errorf("the saved %s names runtime URLs %v, want the fixture's %v", fixture.file, savedRuntime, fixtureRuntime)
			}
			for _, original := range journalRawRuntimeURLs(t, source) {
				if strings.Contains(saved, original) {
					continue
				}
				// A URL holding a bare ampersand comes back as the character reference that
				// spells the same URL; every other address keeps the fixture's own bytes.
				if strings.Contains(original, "&") && strings.Contains(saved, html.EscapeString(original)) {
					t.Logf("%s keeps %s as %s", fixture.file, original, html.EscapeString(original))
					continue
				}
				t.Errorf("the saved %s lost the public runtime URL %s", fixture.file, original)
			}
			fixtureEntries := journalEntries(t, source)
			wantedEntries := append(append([]string{}, fixtureEntries...), entry)
			savedEntries := journalEntries(t, saved)
			if len(savedEntries) != len(wantedEntries) {
				t.Errorf("the saved %s holds %d entry records, want the fixture's %d plus the new one", fixture.file, len(savedEntries), len(fixtureEntries))
			}
			for _, savedEntry := range savedEntries {
				if !slices.Contains(wantedEntries, savedEntry) {
					t.Errorf("the saved %s holds an entry record that is neither the fixture's nor the new one: %s", fixture.file, savedEntry)
				}
			}
			for _, wantedEntry := range wantedEntries {
				if !slices.Contains(savedEntries, wantedEntry) {
					t.Errorf("the saved %s lost the entry record %s", fixture.file, wantedEntry)
				}
			}
			for _, want := range []string{
				"# Welcome to Journal",
				"# Plan for the week",
				"# Notes from the workshop",
				`data-id="welcm1"`,
				"# Written offline in HTML Clay",
				`data-id="j10new1"`,
			} {
				if !strings.Contains(saved, want) {
					t.Errorf("the saved %s lost %q", fixture.file, want)
				}
			}

			again := serve(t, srv, "/"+fixture.file, nil)
			if again.Code != 200 {
				t.Fatalf("serve after the save: %d %s", again.Code, again.Body.String())
			}
			projected, runtime, external := journalURLs(t, again.Body.String())
			if len(projected) == 0 || !slices.Contains(projected, clayruntime.LocalPrefix+clayruntime.Selected+"/sap.js") {
				t.Errorf("the saved %s was not projected when served again: %v", fixture.file, projected)
			}
			if len(runtime) > 0 {
				t.Errorf("the reopened %s still names public runtime URLs: %v", fixture.file, runtime)
			}
			if len(external) > 0 && !slices.Equal(external, journalPreconnects) {
				t.Errorf("the reopened %s keeps external URLs %v", fixture.file, external)
			}
			if !strings.Contains(again.Body.String(), `id="clay-runtime-map"`) {
				t.Errorf("the reopened %s carries no descriptor", fixture.file)
			}
		})
	}
}

var (
	journalEntryOpenPattern = regexp.MustCompile(`<article class="entry"[^>]*>`)
	journalEntryIDPattern   = regexp.MustCompile(`data-id="[^"]+"`)
)

// journalEntries lists every entry record a document holds that carries an id, sliced the way
// the browser sees it: from the opening tag to its closing tag.
func journalEntries(t *testing.T, document string) []string {
	t.Helper()
	entries := []string{}
	for _, opening := range journalEntryOpenPattern.FindAllStringIndex(document, -1) {
		if !journalEntryIDPattern.MatchString(document[opening[0]:opening[1]]) {
			continue
		}
		end := strings.Index(document[opening[0]:], "</article>")
		if end < 0 {
			t.Fatalf("an entry record in the document is not closed")
		}
		entries = append(entries, document[opening[0]:opening[0]+end+len("</article>")])
	}
	return entries
}

// journalEntryTemplate is the first entry record a fixture holds, copied byte for byte.
func journalEntryTemplate(t *testing.T, source string) string {
	t.Helper()
	start := strings.Index(source, `<article class="entry" item`)
	if start < 0 {
		t.Fatal("the fixture holds no entry record to copy")
	}
	end := strings.Index(source[start:], "</article>")
	if end < 0 {
		t.Fatal("the fixture's entry record is not closed")
	}
	return source[start : start+end+len("</article>")]
}

// journalEntryCopy is that record with a new id and new body text, the shape a freshly typed
// entry takes.
func journalEntryCopy(t *testing.T, source string) (string, string) {
	t.Helper()
	template := journalEntryTemplate(t, source)
	entry := strings.Replace(template, `data-id=""`, `data-id="j10new1"`, 1)
	if entry == template {
		t.Fatalf("the copied entry record kept its empty data-id: %s", template)
	}
	empty := `<code class="entry-body" bind="body"></code>`
	entry = strings.Replace(entry, empty, `<code class="entry-body" bind="body"># Written offline in HTML Clay</code>`, 1)
	if !strings.Contains(entry, "# Written offline in HTML Clay") {
		t.Fatalf("the copied entry record has no body to fill: %s", template)
	}
	return template, entry
}

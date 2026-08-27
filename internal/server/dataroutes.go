package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"

	"github.com/panphora/htmlclay/internal/dataapi"
	"github.com/panphora/htmlclay/internal/htmlutil"
)

// dataroutes.go is the JSON projection of the serve path. It is deliberately NOT a second read
// pipeline: every authorization decision — lexical validation, the internal and hidden filters, the
// scope gate and its prompt, the capability open, the descriptor-bound RealPath re-check — is made
// by handleServeFile and serveAsset exactly as it is for a plain GET. A data mode only replaces the
// terminal write, once the bytes are already in hand.
//
// The invariant that shape enforces:
//
//	A data response is a JSON projection of exactly the bytes the same request would have READ FROM
//	DISK — the served bytes minus the server-side injections. It can never read a file the plain GET
//	could not, and it answers with the same status, in the same order, after the same prompt.
//
// A copy of the ladder would drift into an existence oracle or a symlink escape within one release,
// so there is one implementation and a mode flag.

// dataFace is which of the two entry points asked for data.
type dataFace int

const (
	faceNone  dataFace = iota
	faceQuery          // GET <page>?data={…}
	faceAPI            // GET /_/api/<path>
)

// dataMode carries the face and, for the query face, the rules text. The rules ride along rather
// than being re-read from the request so the query string is parsed exactly once: parsing it twice
// invites the two reads to disagree about what the caller asked for.
type dataMode struct {
	face  dataFace
	rules string
}

func (m dataMode) active() bool { return m.face != faceNone }

// dataExample is the author-facing hint from the JS hosts, reproduced verbatim.
const dataExample = `?data={title:"h1",items:".item"}`

// dataError is the error body shape both JS hosts use. Parity is defined on (status, error,
// message); details is free-form and deliberately not compared, because the reference carries V8's
// raw JSON.parse text, which Go will never reproduce and which changes across Node releases.
type dataError struct {
	Error   string `json:"error"`
	Message string `json:"message"`
	Details string `json:"details,omitempty"`
	Example string `json:"example,omitempty"`
}

// isExtractable is a LEXICAL test on the request path, decided before any filesystem access, so it
// can never reveal whether a file exists.
func isExtractable(relPath string) bool {
	switch strings.ToLower(filepath.Ext(relPath)) {
	case ".html", ".htmlclay":
		return true
	}
	return false
}

// dataModeForQuery classifies a plain GET as a ?data= request or not. It answers the request itself
// (returning true) only for a malformed data parameter, which is a property of the query string
// alone and so reveals nothing about the path.
//
// A request that is NOT a data request must fall through and be served exactly as it is today. That
// is why an undecodable query on a page with no data key, or a data parameter on a non-HTML path,
// returns faceNone rather than an error.
func (s *Server) dataModeForQuery(w http.ResponseWriter, r *http.Request, relPath string) (dataMode, bool) {
	raw := r.URL.RawQuery
	if raw == "" {
		return dataMode{}, false
	}

	// r.URL.Query() DISCARDS its parse error and returns whatever it could decode, so `?data=%zz`
	// yields a map with no "data" key and the request silently serves HTML with a 200. Read
	// RawQuery through ParseQuery explicitly and keep the error.
	q, err := url.ParseQuery(raw)
	if !q.Has("data") && !(err != nil && rawQueryHasDataKey(raw)) {
		return dataMode{}, false
	}
	if !isExtractable(relPath) {
		// Documented behavior: on a non-HTML path the parameter is ignored and the file is served
		// exactly as today. htmlclay has no extension gate anywhere else, and hyperclay-local
		// ignores the parameter the same way.
		return dataMode{}, false
	}
	if err != nil {
		writeDataError(w, http.StatusBadRequest, dataError{
			Error:   "Invalid extraction rules",
			Message: "Failed to parse extraction rules. Check your JSON syntax.",
			Details: "the query string could not be decoded: " + err.Error(),
			Example: dataExample,
		})
		return dataMode{}, true
	}

	// Duplicate data keys take the first, a documented divergence: the reference 500s, because qs
	// hands it an array and input.substring is not a function.
	rules := q.Get("data")
	if rules == "" {
		writeDataError(w, http.StatusBadRequest, dataError{
			Error:   "Missing data parameter",
			Message: "Please provide extraction rules via ?data= parameter",
			Example: dataExample,
		})
		return dataMode{}, true
	}
	return dataMode{face: faceQuery, rules: rules}, false
}

// rawQueryHasDataKey answers "did the caller mean to send a data parameter" for a query string
// ParseQuery refused. Without it an undecodable `?data=%zz` is indistinguishable from a page with
// no data parameter at all, and the malformed request would quietly serve HTML with a 200.
func rawQueryHasDataKey(raw string) bool {
	for raw != "" {
		var pair string
		pair, raw, _ = strings.Cut(raw, "&")
		if pair == "" {
			continue
		}
		name, _, _ := strings.Cut(pair, "=")
		if key, err := url.QueryUnescape(name); err == nil && key == "data" {
			return true
		}
	}
	return false
}

// handleDataAPI serves GET /_/api/<path>, where the rules come from the document's own
// script[data-rules-name~="api"] tag rather than from the caller.
//
// Bare /_/api is a 400 rather than an index: htmlclay's origin is not one document, since a site is
// a contiguous readable tree and explicitly-opened siblings can share a port. "index.html" is a
// hyperclay-local convention that would resolve to ~/index.html here, and "whichever file created
// the port" would make one URL change meaning with open order.
func (s *Server) handleDataAPI(w http.ResponseWriter, r *http.Request) {
	rawPath := r.PathValue("path")

	// Both the bare route and the wildcard with a trailing slash land here with an empty path.
	if strings.Trim(rawPath, "/") == "" {
		writeDataError(w, http.StatusBadRequest, dataError{
			Error:   "Missing path",
			Message: "Request a file, for example /_/api/notes.htmlclay.",
		})
		return
	}
	if !isExtractable(extractFilePath(rawPath)) {
		// Lexical, decided before any filesystem access, so this cannot be an existence oracle.
		writeDataError(w, http.StatusNotFound, dataError{
			Error:   "Unsupported file type",
			Message: "Only .html and .htmlclay files publish a data API.",
		})
		return
	}

	// ?data= is ignored on this face: it takes its rules from the document.
	s.serveFile(w, r, rawPath, dataMode{face: faceAPI})
}

// writeExtracted is the ONLY place a data response is produced. It runs after every gate, with the
// bytes already read, which is what makes the two faces impossible to use as a read primitive the
// plain GET does not already grant.
func (s *Server) writeExtracted(w http.ResponseWriter, raw []byte, mode dataMode) {
	// The save token is injected on the way OUT of a document response and is normally absent from
	// disk. "Normally" is not an invariant: an external writer, or someone saving a copy of a served
	// page, can put an htmlclaytoken attribute back. A capability must not become extractable
	// because disk happens to contain it.
	doc, err := dataapi.ParseBytes(htmlutil.StripToken(raw))
	if err != nil {
		s.logger.Printf("Data extraction could not parse the document: %v", err)
		writeDataError(w, http.StatusInternalServerError, extractionFailed())
		return
	}

	rules, err := s.rulesFor(doc, mode)
	if err != nil {
		status, body := mapDataError(err, mode.face)
		writeDataError(w, status, body)
		return
	}

	value, err := doc.Extract(rules)
	if err != nil {
		status, body := mapDataError(err, mode.face)
		writeDataError(w, status, body)
		return
	}

	body, err := dataapi.Marshal(value)
	if err != nil {
		s.logger.Printf("Data extraction could not marshal its result: %v", err)
		writeDataError(w, http.StatusInternalServerError, extractionFailed())
		return
	}

	dataHeaders(w)
	w.WriteHeader(http.StatusOK)
	w.Write(body)
}

// rulesFor resolves the rule tree for whichever face asked.
func (s *Server) rulesFor(doc *dataapi.Document, mode dataMode) (dataapi.Value, error) {
	if mode.face == faceQuery {
		return dataapi.ParseRelaxed(mode.rules)
	}

	tag, err := doc.FindRulesIn("api")
	if err != nil {
		return nil, err
	}
	if tag == nil {
		// A missing tag is not an engine error — the status for "this page publishes no such
		// endpoint" is the server's call — so it is turned into one here.
		return nil, errNoAPIRulesTag
	}
	return tag.Rules, nil
}

var errNoAPIRulesTag = errors.New(`this page has no rules tag with data-rules-name~="api"`)

// mapDataError maps by error TYPE, never by sniffing message text. The JS hosts sniff, which is why
// two sibling css-what failures one line apart in the same file answer 400 and 500 there. The
// bodies below reproduce theirs; the classification does not.
func mapDataError(err error, face dataFace) (int, dataError) {
	if face == faceAPI {
		if errors.Is(err, errNoAPIRulesTag) {
			return http.StatusBadRequest, dataError{
				Error:   "No api rules tag",
				Message: `This page has no rules tag with data-rules-name~="api".`,
			}
		}
		var version *dataapi.UnknownRulesVersion
		if errors.As(err, &version) {
			return http.StatusBadRequest, dataError{
				Error:   "Unsupported rules version",
				Message: err.Error(),
			}
		}
		var parse *dataapi.RulesParseError
		if errors.As(err, &parse) {
			return http.StatusBadRequest, dataError{
				Error:   "Malformed api rules tag",
				Message: "The api rules tag body is not valid JSON.",
				Details: err.Error(),
			}
		}
		var selector dataapi.SelectorFailure
		if errors.As(err, &selector) {
			return http.StatusBadRequest, dataError{
				Error:   "Invalid selector in api rules tag",
				Message: err.Error(),
			}
		}
		return http.StatusInternalServerError, extractionFailed()
	}

	var parse *dataapi.RulesParseError
	if errors.As(err, &parse) {
		return http.StatusBadRequest, dataError{
			Error:   "Invalid extraction rules",
			Message: "Failed to parse extraction rules. Check your JSON syntax.",
			Details: err.Error(),
			Example: dataExample,
		}
	}
	var selector dataapi.SelectorFailure
	if errors.As(err, &selector) {
		return http.StatusBadRequest, dataError{
			Error:   "Invalid CSS selector",
			Message: "One or more CSS selectors are invalid",
			Details: err.Error(),
		}
	}

	// Everything left is a rule depth overflow, which is a 500 on both JS hosts today: its message
	// carries neither "JSON" nor "selector", so their sniffing misses it. Kept at 500 for parity,
	// with the upstream fix moving all four to 400.
	return http.StatusInternalServerError, extractionFailed()
}

func extractionFailed() dataError {
	return dataError{Error: "Extraction failed", Message: "Failed to extract data from the site"}
}

// dataHeaders are set on every extraction-generated response, success or error. Responses INHERITED
// from the serve path keep their own shape: the out-of-scope 403 stays application/problem+json and
// the shared 404s stay http.Error, which is what keeps the data faces from becoming the
// hidden-versus-missing oracle the plain GET path was built not to be.
//
// No CORS, deliberately. The platform's /_/api is public and CORS-enabled because those pages are
// already world-readable; on loopback the same bytes are a private home-directory file. This is
// more load-bearing than it looks: security.go rejects only the exact string "cross-site", and every
// http://localhost:* origin is SAME-site with htmlclay's, so any other local dev server can issue
// the request and the absence of Access-Control-Allow-* is the only thing stopping it from reading
// the JSON. A wildcard here would punch a hole in the control that protects saving. curl is
// unaffected, since it sends no such header.
func dataHeaders(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
}

func writeDataError(w http.ResponseWriter, status int, body dataError) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	// Match the success path, which reproduces JSON.stringify: Go escapes <, > and & by default and
	// JavaScript does not, and a rejected selector routinely contains all three.
	enc.SetEscapeHTML(false)
	if err := enc.Encode(body); err != nil {
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	dataHeaders(w)
	w.WriteHeader(status)
	w.Write(bytes.TrimRight(buf.Bytes(), "\n"))
}

package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/panphora/htmlclay/internal/config"
	"github.com/panphora/htmlclay/internal/session"
)

// The wire CLI is a client of a RUNNING HTML Clay, never a second instance of
// one. It binds no port, takes no single-instance lock, opens no file, and does
// everything through the two routes in internal/server/wire.go over loopback. That is
// what keeps `wire serve` inside the app's constraints: the handler runs in the
// terminal the user launched it from, so HTML Clay gains no responsibility for
// models, credentials, or long-lived processes.
//
// It sends no Origin and no Sec-Fetch-Site, and must never start: attesting
// nothing at all is exactly how the wire guard recognizes a local process.
const (
	wireExitOK = 0
	// Bad arguments, an unknown subcommand, a path that cannot be resolved.
	wireExitUsage = 1
	// Nothing is listening on any remembered port: the app is not running.
	wireExitAppDown = 2
	// The address is held by the parked recovery listener (recovery.go), so the
	// app is running but nothing is open at this origin.
	wireExitRecovery = 3
	// A live site answered, and no site knows this file.
	wireExitNoFile = 4
	// 409: another process already holds the exclusive handler slot.
	wireExitHandlerTaken = 5
	// Any other refusal from a site that did resolve the file (429, 403, 503).
	wireExitRefused = 6
	// The send was accepted and no handler was attached to take it.
	wireExitNotDelivered = 7
)

// wireServeMax bounds requests in flight for one `wire serve`. A handler is a
// process the user is paying for; running an unbounded number of them because a
// page looped is the failure this prevents. Past it a request is refused with a
// terminal frame rather than queued, so the page stops spinning.
const wireServeMax = 8

// wireMaxFrame bounds one SSE frame. The server caps a send body at 1 MiB
// (maxWireBody), and the frame it emits is that body re-stamped with v, from and
// the absolute file path, so the slack has to cover a long path rather than a few
// bytes of framing.
const wireMaxFrame = (1 << 20) + (1 << 16)

// wireMaxStatus matches the server's own status-text cap (maxWireText). A longer
// line is truncated here rather than sent and truncated there, so what the page
// sees and what the CLI logged agree.
const wireMaxStatus = 4 << 10

// wireIdleTimeout ends a stream that has gone silent. The server sends a
// keepalive comment every 25s, so silence past three of them means the
// connection is dead in a way TCP has not noticed, or the port is held by
// something that answered like a wire and then said nothing. Without this the
// reader blocks forever and never reconnects, which is worse than a wrong port:
// it never even retries.
const wireIdleTimeout = 90 * time.Second

// wireChildDrain is how long a finished request's output is still collected
// before the pipe is forced shut. It only matters when a descendant of the
// handler inherited stdout and outlived it; see wireServer.run.
const wireChildDrain = 2 * time.Second

type wireEnv struct {
	configPath string
	// localRootsPath is Hyperclay Local's served-roots.json. Empty disables the lookup.
	localRootsPath string
	stdin          io.Reader
	stdout         io.Writer
	stderr         io.Writer
	// ctx is what ends a long-running subcommand. runWire installs a
	// signal-cancelled one when a caller leaves it nil; a test supplies its own,
	// because the alternative is signalling the test binary itself.
	ctx context.Context
}

// wireFrame is the CLI's view of the protocol. It deliberately mirrors rather
// than shares the server's envelope: a client must tolerate a field it does not
// know instead of failing on it, and Payload is carried as raw JSON that the CLI
// never inspects.
//
// V, From and File are stamped server-side on send and whatever is put here for
// the first two is discarded. File is the exception and is required: a local
// process has no page, so naming the absolute path is how it addresses a file.
type wireFrame struct {
	V       int             `json:"v,omitempty"`
	Type    string          `json:"type"`
	ID      string          `json:"id"`
	From    string          `json:"from,omitempty"`
	File    string          `json:"file,omitempty"`
	Text    string          `json:"text,omitempty"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

const wireUsage = `htmlclay wire — talk to one open HTML Clay file from this terminal

  htmlclay wire serve  <file> [--protocol=jsonl] -- <cmd> [args...]
                                                   run <cmd> for every request
  htmlclay wire listen <file> [--handler]          print frames as JSON lines
  htmlclay wire send   <file> --type <type> ...    send one frame, payload on stdin
  htmlclay wire where  <file>                      print the origin serving <file>

Flags may come before or after <file>. Every command also takes --port <n> to
name the origin directly, which is how you reach a file whose folder HTML Clay
does not remember a port for (a file loose in your home directory, say): read the
port off the page's own URL.

serve runs <cmd> once per wire/request with the request envelope on stdin, and
sets HTMLCLAY_WIRE_FILE and HTMLCLAY_WIRE_ID in its environment. Each line the
command prints becomes a wire/status; exiting 0 becomes wire/done and any other
exit becomes wire/error. A wire/cancel stops the command it names.

serve --protocol=jsonl requires JSON Lines helper output, returns result payloads,
and runs the helper from the document's directory with a five minute deadline.

listen is an observer unless --handler is given. The handler slot is exclusive
and also keeps HTML Clay watching the file while no tab is open on it.

Frames go to stdout, one JSON object per line. Everything else goes to stderr.

Exit codes: 1 usage, 2 HTML Clay is not running, 3 the address is held by the
recovery page, 4 no site is serving that file, 5 the handler slot is taken,
6 refused, 7 sent with no handler attached, 8 both HTML Clay and Hyperclay Local
serve that file, so pass --port.
`

// wireDispatch runs a `htmlclay wire ...` invocation and reports whether it
// handled argv. It is called at the very top of main(), before flag.Parse and
// therefore before initConfig takes the single-instance lock: flag.Parse stops
// at the first non-flag argument, so `htmlclay wire listen --handler x` would
// otherwise leave --handler unparsed, and with no dispatch at all `wire` reaches
// openFile as a path.
func wireDispatch(argv []string) (int, bool) {
	if len(argv) < 2 || argv[1] != "wire" {
		return 0, false
	}
	path, err := config.Path()
	if err != nil {
		fmt.Fprintf(os.Stderr, "htmlclay wire: cannot resolve the config file: %v\n", err)
		return wireExitUsage, true
	}
	env := &wireEnv{configPath: path, localRootsPath: wireLocalRootsPath(), stdin: os.Stdin, stdout: os.Stdout, stderr: os.Stderr}
	return runWire(env, argv[2:]), true
}

func runWire(env *wireEnv, args []string) int {
	if len(args) == 0 {
		fmt.Fprint(env.stderr, wireUsage)
		return wireExitUsage
	}
	if env.ctx == nil {
		ctx, stop := signal.NotifyContext(context.Background(), wireStopSignals...)
		defer stop()
		env.ctx = ctx
	}
	switch args[0] {
	case "serve":
		return wireServeCmd(env, args[1:])
	case "listen":
		return wireListenCmd(env, args[1:])
	case "send":
		return wireSendCmd(env, args[1:])
	case "where":
		return wireWhereCmd(env, args[1:])
	case "help", "-h", "--help":
		fmt.Fprint(env.stdout, wireUsage)
		return wireExitOK
	default:
		fmt.Fprintf(env.stderr, "htmlclay wire: unknown command %q\n\n%s", args[0], wireUsage)
		return wireExitUsage
	}
}

func wireFlagSet(env *wireEnv, name string) *flag.FlagSet {
	fs := flag.NewFlagSet("wire "+name, flag.ContinueOnError)
	fs.SetOutput(env.stderr)
	fs.Usage = func() {
		fmt.Fprint(env.stderr, wireUsage)
	}
	return fs
}

// wireParse parses flags that may sit on either side of the file argument and
// returns the non-flag arguments.
//
// A plain fs.Parse stops at the first non-flag argument, so `wire send <file>
// --type wire/request` would leave --type unparsed and the file argument check
// would then reject three arguments where it wanted one. That is the documented
// form, and it must work: putting the file last is the shape nobody types.
// Parsing, taking one operand, and parsing the rest is the standard way to get
// interspersed flags out of the flag package, and it keeps flag's own handling
// of `--type X` versus `--type=X`.
func wireParse(fs *flag.FlagSet, args []string) ([]string, error) {
	var operands []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		rest := fs.Args()
		if len(rest) == 0 {
			return operands, nil
		}
		operands = append(operands, rest[0])
		args = rest[1:]
	}
}

// wireTargetPath turns a command-line path into the absolute, symlink-resolved
// one the session manager registered. Resolution is not cosmetic: files are
// looked up by their resolved path (openFile does the same before routing), so
// an unresolved spelling of a served file is simply not found.
//
// A file that does not exist still resolves through its directory, because the
// handler role is what turns version history on for a file an agent is about to
// create, and refusing it here would make that impossible.
func wireTargetPath(raw string) (string, error) {
	abs, err := filepath.Abs(raw)
	if err != nil {
		return "", err
	}
	if resolved, rErr := filepath.EvalSymlinks(abs); rErr == nil {
		return filepath.Clean(resolved), nil
	}
	dir, base := filepath.Split(abs)
	if resolved, rErr := filepath.EvalSymlinks(filepath.Clean(dir)); rErr == nil {
		return filepath.Join(resolved, base), nil
	}
	return filepath.Clean(abs), nil
}

func wireFileArg(env *wireEnv, args []string) (string, int) {
	if len(args) != 1 {
		fmt.Fprint(env.stderr, wireUsage)
		return "", wireExitUsage
	}
	file, err := wireTargetPath(args[0])
	if err != nil {
		fmt.Fprintf(env.stderr, "htmlclay wire: cannot resolve %s: %v\n", args[0], err)
		return "", wireExitUsage
	}
	return file, 0
}

// newWireID mints an opaque request id. The router validates that one exists and
// is bounded and otherwise never interprets it, and the page mints its own with
// crypto.randomUUID(), so this matches that format rather than inventing a
// second one.
func newWireID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("cli-%d", time.Now().UnixNano())
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// --- discovery -------------------------------------------------------------

type wireCandidate struct {
	anchor string
	port   int
	// ancestor records that this port's anchor contains the file, which is what
	// makes a recovery page found here mean "this file's own address is parked"
	// rather than "some unrelated address is".
	ancestor bool
	// source names the app that published this anchor ("htmlclay" or
	// "hyperclay-local"), which is what the ambiguity check groups by. Empty on
	// a --port candidate.
	source string
}

var (
	errWireAppDown  = errors.New("HTML Clay is not running, or has never served this file's folder")
	errWireNoFile   = errors.New("no HTML Clay site is serving this file; open it first")
	errWireRecovery = errors.New("this address is held by HTML Clay's recovery page, so nothing is open here")

	errWireAmbiguous = errors.New("both HTML Clay and Hyperclay Local serve this file")
)

// 8: both apps serve the file, and only --port can say which origin is meant.
const wireExitAmbiguous = 8

// wireSitePorts reads the remembered ports WITHOUT going through config.Load.
//
// Load renames a corrupt config aside, promotes legacy fields, dedupes and caps,
// so a `htmlclay wire` invocation racing the running app could quarantine the
// live config. A tolerant decode of the one field this CLI needs cannot. Every
// failure here means no hint at all, and --port is the way out of that.
func wireSitePorts(configPath string) map[string]int {
	data, err := os.ReadFile(configPath)
	if err != nil {
		return nil
	}
	var doc struct {
		SitePorts map[string]int `json:"sitePorts"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil
	}
	return doc.SitePorts
}

// wireLocalRootsPath is where Hyperclay Local publishes the folders it serves.
// The dev build appends -dev to that folder and is deliberately not read: a dev
// build is reached with --port instead.
func wireLocalRootsPath() string {
	base, err := os.UserConfigDir()
	if err != nil {
		return ""
	}
	return filepath.Join(base, "HyperclayLocal", "served-roots.json")
}

type wireLocalRoot struct {
	Path string `json:"path"`
	Port int    `json:"port"`
}

// wireLocalRoots reads Hyperclay Local's served folders, tolerantly, and only
// while the process that wrote them is still running. Every failure means no
// hint, exactly like wireSitePorts.
func wireLocalRoots(path string) []wireLocalRoot {
	if path == "" {
		return nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var doc struct {
		V     int             `json:"v"`
		PID   int             `json:"pid"`
		Roots []wireLocalRoot `json:"roots"`
	}
	if json.Unmarshal(data, &doc) != nil || doc.V != 1 || !wirePIDAlive(doc.PID) {
		return nil
	}
	return doc.Roots
}

func wirePIDAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	if runtime.GOOS == "windows" {
		p.Release()
		return true
	}
	err = p.Signal(syscall.Signal(0))
	return err == nil || errors.Is(err, syscall.EPERM)
}

// wireCandidates orders the remembered ports by how likely each is to serve
// file. This is a PROBE ORDER and never a decision: the anchor rule (the
// broadest live trusted folder wins, with an identity pin and a lexical
// tiebreak) lives in sites.go, and reimplementing it here is how a bug family
// gets a second home. The server answers; this only decides who to ask first.
//
// Ancestors first and broadest first mirrors that rule, so the ordinary case is
// one request. Every other remembered port follows, because config.json is
// written only when a port changes and a merely stale hint must not make a live
// origin unreachable. A site never admits a file another site registered, so a
// wrong guess costs one 404 and nothing else.
//
// KNOWN GAP, and --port is the answer to it: a file whose origin anchors at the
// home directory has no entry at all, because rememberPort deliberately skips
// that anchor (every loose file would otherwise overwrite one entry, and startup
// would then bind a port belonging to whichever file was opened last). Such a
// file is served and editable but undiscoverable from here.
func wireCandidates(ports map[string]int, file string) []wireCandidate {
	dir := filepath.Dir(file)
	var ancestors, others []wireCandidate
	for anchor, port := range ports {
		if port <= 0 || port > 65535 {
			continue
		}
		if session.EqualOrUnder(dir, anchor) {
			ancestors = append(ancestors, wireCandidate{anchor: anchor, port: port, ancestor: true, source: "htmlclay"})
		} else {
			others = append(others, wireCandidate{anchor: anchor, port: port, source: "htmlclay"})
		}
	}
	sort.Slice(ancestors, func(i, j int) bool {
		if len(ancestors[i].anchor) != len(ancestors[j].anchor) {
			return len(ancestors[i].anchor) < len(ancestors[j].anchor)
		}
		return ancestors[i].anchor < ancestors[j].anchor
	})
	sort.Slice(others, func(i, j int) bool { return others[i].anchor < others[j].anchor })
	return append(ancestors, others...)
}

// wireAllCandidates merges both apps' anchors into one probe order: every
// ancestor from either app broadest first, then every other anchor. A file only
// one app serves costs at most one 404 from the other.
func wireAllCandidates(ports map[string]int, local []wireLocalRoot, file string) []wireCandidate {
	cands := wireCandidates(ports, file)
	dir := filepath.Dir(file)
	var ancestors, others []wireCandidate
	for _, c := range cands {
		if c.ancestor {
			ancestors = append(ancestors, c)
		} else {
			others = append(others, c)
		}
	}
	for _, r := range local {
		if r.Port <= 0 || r.Port > 65535 || r.Path == "" {
			continue
		}
		c := wireCandidate{anchor: r.Path, port: r.Port, source: "hyperclay-local"}
		if session.EqualOrUnder(dir, r.Path) {
			c.ancestor = true
			ancestors = append(ancestors, c)
		} else {
			others = append(others, c)
		}
	}
	sort.SliceStable(ancestors, func(i, j int) bool {
		if len(ancestors[i].anchor) != len(ancestors[j].anchor) {
			return len(ancestors[i].anchor) < len(ancestors[j].anchor)
		}
		return ancestors[i].anchor < ancestors[j].anchor
	})
	sort.SliceStable(others, func(i, j int) bool { return others[i].anchor < others[j].anchor })
	return append(ancestors, others...)
}

// wireResolveCandidates is what every subcommand calls. A --port names the
// origin outright and suppresses the hint entirely.
func wireResolveCandidates(env *wireEnv, file string, port int) []wireCandidate {
	if port > 0 {
		return []wireCandidate{{anchor: "--port", port: port, ancestor: true}}
	}
	return wireAllCandidates(wireSitePorts(env.configPath), wireLocalRoots(env.localRootsPath), file)
}

// wireAmbiguity asks the best ancestor of each app, as an observer, whether it
// serves file. Both answering is the one case the CLI cannot settle by probing,
// because each app answers truthfully for its own server.
func wireAmbiguity(ctx context.Context, cands []wireCandidate, file string) error {
	first := map[string]wireCandidate{}
	for _, c := range cands {
		if !c.ancestor || c.source == "" {
			continue
		}
		if _, seen := first[c.source]; !seen {
			first[c.source] = c
		}
	}
	h, okH := first["htmlclay"]
	l, okL := first["hyperclay-local"]
	if !okH || !okL {
		return nil
	}
	serves := func(c wireCandidate) bool {
		pctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		resp, err := wireSubscribe(pctx, c, file, false, "", "")
		if err != nil {
			return false
		}
		defer resp.Body.Close()
		return wireClassify(resp, "text/event-stream") == wireHit
	}
	if serves(h) && serves(l) {
		return fmt.Errorf("%w: HTML Clay on port %d and Hyperclay Local on port %d; pass --port to choose one", errWireAmbiguous, h.port, l.port)
	}
	return nil
}

type wireVerdict int

const (
	wireMiss      wireVerdict = iota // a live site that does not know this file
	wireHit                          // this origin owns the file and answered
	wireRefused                      // a site answered, but not with success
	wireParked                       // the recovery listener holds this port
	wireUnrelated                    // something that is not HTML Clay holds this port
)

// wireClassify reads status and content type as an answer about WHICH origin
// this is, before anything reads a body.
//
// A 404 is the one status two different things send. The parked recovery
// listener answers 404 text/html on every path, while a live site answers 404
// as plain text on subscribe and as JSON on send, so the content type separates
// them without the CLI knowing which port is parked.
//
// Anything else that is not a clean success is REFUSED rather than accepted as
// the answer, because a remembered port can be held by a stranger: a 500 from an
// unrelated server on a broader anchor would otherwise stop the walk and be
// reported as this file's own origin refusing, while the real origin sits
// unasked on the next candidate. wireTry keeps the first refusal and prefers any
// later success.
//
// A redirect is never followed (see the clients below) and a 200 of the wrong
// content type is a stranger too: taking either for the origin would hang a
// stream reader on bytes that never arrive.
func wireClassify(resp *http.Response, wantType string) wireVerdict {
	if resp.StatusCode == http.StatusNotFound {
		if strings.Contains(resp.Header.Get("Content-Type"), "text/html") {
			return wireParked
		}
		return wireMiss
	}
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		return wireUnrelated
	}
	if resp.StatusCode == http.StatusOK {
		if !strings.Contains(resp.Header.Get("Content-Type"), wantType) {
			return wireUnrelated
		}
		return wireHit
	}
	return wireRefused
}

// wireTry asks each candidate in turn and returns the one that owns the file,
// with its response still OPEN so a stream can keep reading it.
//
// It walks the whole list rather than stopping at the first answer that is not a
// 404, so one stranger on a remembered port cannot hide the live origin behind
// it. A refusal is remembered and returned only when nothing better turns up.
func wireTry(cands []wireCandidate, wantType string, attempt func(c wireCandidate) (*http.Response, error)) (*http.Response, wireCandidate, error) {
	var sawLive, sawParked, sawParkedAncestor bool
	var refused *http.Response
	var refusedCand wireCandidate

	for _, c := range cands {
		resp, err := attempt(c)
		if err != nil {
			continue // nothing listening, or it went away between config and now
		}
		switch wireClassify(resp, wantType) {
		case wireHit:
			if refused != nil {
				refused.Body.Close()
			}
			return resp, c, nil
		case wireRefused:
			if refused == nil {
				refused, refusedCand = resp, c
				continue // deliberately left open; it is the fallback answer
			}
		case wireParked:
			sawParked = true
			sawParkedAncestor = sawParkedAncestor || c.ancestor
		case wireMiss:
			sawLive = true
		}
		resp.Body.Close()
	}

	if refused != nil {
		return refused, refusedCand, nil
	}
	switch {
	// A recovery page on a folder that CONTAINS the file is the specific answer:
	// this file's own address is held and nothing is open there. An unrelated
	// site's 404 is the general one, so it only wins when no ancestor is parked.
	case sawParkedAncestor:
		return nil, wireCandidate{}, errWireRecovery
	case sawLive:
		return nil, wireCandidate{}, errWireNoFile
	case sawParked:
		return nil, wireCandidate{}, errWireRecovery
	default:
		return nil, wireCandidate{}, errWireAppDown
	}
}

func wireReport(env *wireEnv, file string, err error) int {
	fmt.Fprintf(env.stderr, "htmlclay wire: %v (%s)\n", err, file)
	switch {
	case errors.Is(err, errWireNoFile):
		return wireExitNoFile
	case errors.Is(err, errWireRecovery):
		return wireExitRecovery
	case errors.Is(err, errWireAmbiguous):
		return wireExitAmbiguous
	default:
		fmt.Fprintf(env.stderr, "htmlclay wire: if the page is open, its port is in the browser's address bar; pass it with --port\n")
		return wireExitAppDown
	}
}

// wireStatusExit maps a refusal from the file's own origin onto an exit code, so
// a script can tell "the slot is taken" from "that file is not open".
func wireStatusExit(code int) int {
	switch code {
	case http.StatusConflict:
		return wireExitHandlerTaken
	case http.StatusNotFound:
		return wireExitNoFile
	default:
		return wireExitRefused
	}
}

// --- transport -------------------------------------------------------------

// wireNoRedirects refuses to follow a redirect and hands the 3xx back instead.
//
// This is not hygiene. A remembered port can be held by a stranger, and Go's
// default client would follow its 307 off loopback entirely, REPOSTING the
// envelope (the absolute path of a file on this machine, and whatever payload
// the page sent) to a remote host. Nothing in this protocol ever redirects, so
// the only redirect the CLI can meet is one it must not obey.
func wireNoRedirects(*http.Request, []*http.Request) error {
	return http.ErrUseLastResponse
}

func wireTransport() *http.Transport {
	return &http.Transport{
		DialContext:           (&net.Dialer{Timeout: 2 * time.Second}).DialContext,
		ResponseHeaderTimeout: 5 * time.Second,
		DisableCompression:    true,
	}
}

// wireProbeClient is for requests that complete: a probe and a send. Its
// timeouts are short because every candidate is on loopback.
var wireProbeClient = &http.Client{
	Timeout:       10 * time.Second,
	Transport:     wireTransport(),
	CheckRedirect: wireNoRedirects,
}

// wireStreamClient has no overall timeout: a wire is idle between requests by
// design. What bounds it instead is wireIdleTimeout, applied by the caller,
// because the server's keepalive is the only proof a silent stream is alive.
var wireStreamClient = &http.Client{
	Transport:     wireTransport(),
	CheckRedirect: wireNoRedirects,
}

func wireSubscribeURL(port int, file string, handler bool, mode string) string {
	q := url.Values{"file": {file}}
	if handler {
		q.Set("role", "handler")
		if mode != "" {
			q.Set("mode", mode)
		}
	}
	return fmt.Sprintf("http://127.0.0.1:%d/_/wire/subscribe?%s", port, q.Encode())
}

func wireSubscribe(ctx context.Context, c wireCandidate, file string, handler bool, lastID, mode string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, wireSubscribeURL(c.port, file, handler, mode), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "text/event-stream")
	if lastID != "" {
		req.Header.Set("Last-Event-ID", lastID)
	}
	return wireStreamClient.Do(req)
}

// wirePost sends one frame. The body is rebuilt per candidate so a retry after a
// miss has a readable body.
func wirePost(ctx context.Context, c wireCandidate, body []byte) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		fmt.Sprintf("http://127.0.0.1:%d/_/wire/send", c.port), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	return wireProbeClient.Do(req)
}

// wireDrain empties a response that is being discarded, so the connection goes
// back to the pool instead of being torn down. A chatty handler posts one frame
// per line, and without this each one costs a fresh TCP connection.
func wireDrain(resp *http.Response) {
	io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	resp.Body.Close()
}

// --- the stream loop -------------------------------------------------------

type wireStreamOpts struct {
	file    string
	port    int
	handler bool
	mode    string
	onFrame func(raw []byte, f wireFrame)
}

// wireIdleReader notes that bytes arrived, so a watchdog can tell a live but
// quiet stream from a dead one.
type wireIdleReader struct {
	r     io.Reader
	awake func()
}

func (w *wireIdleReader) Read(p []byte) (int, error) {
	n, err := w.r.Read(p)
	if n > 0 {
		w.awake()
	}
	return n, err
}

// wireStream keeps one subscription alive and hands every frame to onFrame.
//
// The origin is re-resolved on EVERY attempt and never cached. Untrusting a
// folder re-homes its opened files onto a different site with a fresh port, and
// a cached port would reconnect to a listener that no longer owns the file.
//
// The FIRST attempt fails fast, so `wire listen` on a file nobody has open exits
// with a usable code instead of spinning. Once a subscription has been
// established every later failure retries with backoff, because the whole point
// of the handler role is to outlive the tabs.
func wireStream(ctx context.Context, env *wireEnv, opts wireStreamOpts) int {
	const (
		minBackoff = 250 * time.Millisecond
		maxBackoff = 5 * time.Second
	)
	role := "observer"
	if opts.handler {
		role = "handler"
	}
	backoff := minBackoff
	lastID := ""
	attached := false
	checked := false

	for {
		// Each attempt gets its own cancellable context and a watchdog armed on
		// it, so a connection that stops delivering bytes is dropped and retried
		// instead of blocking the reader forever.
		attemptCtx, attemptCancel := context.WithCancel(ctx)
		idle := time.AfterFunc(wireIdleTimeout, attemptCancel)
		stop := func() {
			idle.Stop()
			attemptCancel()
		}

		cands := wireResolveCandidates(env, opts.file, opts.port)
		if !checked {
			// Once, before the first connect: the answer cannot change while
			// this stream is open, and a reconnect must not spend two extra
			// probes on every attempt.
			checked = true
			if opts.port == 0 {
				if err := wireAmbiguity(ctx, cands, opts.file); err != nil {
					stop()
					return wireReport(env, opts.file, err)
				}
			}
		}
		resp, c, err := wireTry(cands, "text/event-stream", func(c wireCandidate) (*http.Response, error) {
			return wireSubscribe(attemptCtx, c, opts.file, opts.handler, lastID, opts.mode)
		})

		switch {
		case err != nil:
			stop()
			if ctx.Err() != nil {
				return wireExitOK
			}
			if !attached {
				return wireReport(env, opts.file, err)
			}
			fmt.Fprintf(env.stderr, "[wire] %v; retrying\n", err)

		case resp.StatusCode != http.StatusOK:
			status := resp.StatusCode
			wireDrain(resp)
			stop()
			if !attached {
				fmt.Fprintf(env.stderr, "htmlclay wire: %s refused the %s subscription: %s\n",
					c.anchor, role, http.StatusText(status))
				return wireStatusExit(status)
			}
			fmt.Fprintf(env.stderr, "[wire] %s refused the %s subscription: %s; retrying\n",
				c.anchor, role, http.StatusText(status))

		default:
			attached = true
			backoff = minBackoff
			fmt.Fprintf(env.stderr, "[wire] attached as %s to http://127.0.0.1:%d (%s)\n", role, c.port, opts.file)
			reader := &wireIdleReader{r: resp.Body, awake: func() { idle.Reset(wireIdleTimeout) }}
			lastID = wireReadStream(reader, lastID, opts.onFrame)
			resp.Body.Close()
			stop()
			if ctx.Err() != nil {
				return wireExitOK
			}
			fmt.Fprintf(env.stderr, "[wire] stream closed; reconnecting\n")
		}

		select {
		case <-ctx.Done():
			return wireExitOK
		case <-time.After(backoff):
		}
		if backoff *= 2; backoff > maxBackoff {
			backoff = maxBackoff
		}
	}
}

// wireReadStream parses SSE frames until the stream ends and returns the last id
// it saw, which is what a reconnect resumes from.
//
// The id is committed only once its own frame has been DELIVERED. The id line
// arrives before the data line, so committing on sight would mean a connection
// dropped between the two lines reconnects past a frame it never received, and
// the server replays only what is above the resume point: a lost wire/done is
// then lost permanently, and a page waiting on it waits forever.
func wireReadStream(body io.Reader, lastID string, onFrame func(raw []byte, f wireFrame)) string {
	sc := bufio.NewScanner(body)
	sc.Buffer(make([]byte, 0, 64<<10), wireMaxFrame)
	id := lastID
	pending := lastID
	var data []string
	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "":
			if len(data) == 0 {
				continue
			}
			raw := []byte(strings.Join(data, "\n"))
			data = data[:0]
			var f wireFrame
			if err := json.Unmarshal(raw, &f); err != nil {
				continue
			}
			onFrame(raw, f)
			id = pending
		case strings.HasPrefix(line, ":"):
			// A keepalive comment. The wire is idle between requests, so this is
			// most of what travels on it.
		case strings.HasPrefix(line, "id:"):
			pending = strings.TrimSpace(strings.TrimPrefix(line, "id:"))
		case strings.HasPrefix(line, "data:"):
			data = append(data, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		}
	}
	return id
}

// --- where -----------------------------------------------------------------

func wireWhereCmd(env *wireEnv, args []string) int {
	fs := wireFlagSet(env, "where")
	port := fs.Int("port", 0, "the origin's loopback port, instead of looking one up")
	operands, err := wireParse(fs, args)
	if err != nil {
		return wireExitUsage
	}
	file, code := wireFileArg(env, operands)
	if code != 0 {
		return code
	}

	ctx, cancel := context.WithTimeout(env.ctx, 15*time.Second)
	defer cancel()

	cands := wireResolveCandidates(env, file, *port)
	if *port == 0 {
		if err := wireAmbiguity(ctx, cands, file); err != nil {
			return wireReport(env, file, err)
		}
	}
	resp, c, err := wireTry(cands, "text/event-stream", func(c wireCandidate) (*http.Response, error) {
		// An observer, always. A handler probe would take the exclusive slot and
		// the watch lease from whatever is meant to hold them.
		return wireSubscribe(ctx, c, file, false, "", "")
	})
	if err != nil {
		if ctx.Err() != nil {
			// Every candidate failed because the walk itself was cut short, which
			// says nothing about whether the app is running.
			fmt.Fprintf(env.stderr, "htmlclay wire: gave up looking for %s\n", file)
			return wireExitRefused
		}
		return wireReport(env, file, err)
	}
	// Only the headers were ever wanted, and this is the one response that must
	// NOT be drained first. A successful probe's body is a live SSE stream that
	// never ends, so reading it to discard blocks until this command's own
	// deadline: `where` would answer correctly, fifteen seconds late. Closing is
	// the whole point here, since it ends the subscription immediately.
	resp.Body.Close()

	out := map[string]any{
		"file":   file,
		"origin": fmt.Sprintf("http://127.0.0.1:%d", c.port),
		"anchor": c.anchor,
		"state":  "live",
	}
	if resp.StatusCode != http.StatusOK {
		out["state"] = "refused"
		out["status"] = resp.StatusCode
	}
	line, _ := json.Marshal(out)
	fmt.Fprintf(env.stdout, "%s\n", line)
	if resp.StatusCode != http.StatusOK {
		return wireStatusExit(resp.StatusCode)
	}
	return wireExitOK
}

// --- listen ----------------------------------------------------------------

func wireListenCmd(env *wireEnv, args []string) int {
	fs := wireFlagSet(env, "listen")
	handler := fs.Bool("handler", false, "take the exclusive handler slot and keep the file watched")
	port := fs.Int("port", 0, "the origin's loopback port, instead of looking one up")
	operands, err := wireParse(fs, args)
	if err != nil {
		return wireExitUsage
	}
	file, code := wireFileArg(env, operands)
	if code != 0 {
		return code
	}

	out := bufio.NewWriter(env.stdout)
	defer out.Flush()
	return wireStream(env.ctx, env, wireStreamOpts{
		file:    file,
		port:    *port,
		handler: *handler,
		onFrame: func(raw []byte, _ wireFrame) {
			// Verbatim where it can be: the envelope on stdout is this CLI's
			// public contract, and re-encoding it would let a field the CLI does
			// not model disappear. A frame whose JSON spans lines is compacted
			// instead, because one object per line is the other half of that
			// contract and a reader splitting on newlines has to keep working.
			out.Write(wireOneLine(raw))
			out.WriteByte('\n')
			out.Flush()
		},
	})
}

func wireOneLine(raw []byte) []byte {
	if !bytes.ContainsAny(raw, "\n\r") {
		return raw
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		return bytes.ReplaceAll(bytes.ReplaceAll(raw, []byte("\r"), nil), []byte("\n"), []byte(" "))
	}
	return buf.Bytes()
}

// --- send ------------------------------------------------------------------

func wireSendCmd(env *wireEnv, args []string) int {
	fs := wireFlagSet(env, "send")
	typ := fs.String("type", "", `frame type, e.g. "wire/request"`)
	id := fs.String("id", "", "request id (default: a fresh uuid)")
	text := fs.String("text", "", "free-form text")
	port := fs.Int("port", 0, "the origin's loopback port, instead of looking one up")
	operands, err := wireParse(fs, args)
	if err != nil {
		return wireExitUsage
	}
	file, code := wireFileArg(env, operands)
	if code != 0 {
		return code
	}
	if !strings.HasPrefix(*typ, "wire/") {
		fmt.Fprintf(env.stderr, "htmlclay wire send: --type must start with \"wire/\" (got %q)\n", *typ)
		return wireExitUsage
	}

	payload, err := wireReadPayload(env)
	if err != nil {
		fmt.Fprintf(env.stderr, "htmlclay wire send: %v\n", err)
		if env.ctx.Err() != nil {
			return wireExitOK
		}
		return wireExitUsage
	}

	frame := wireFrame{Type: *typ, ID: *id, File: file, Text: *text, Payload: payload}
	if frame.ID == "" {
		frame.ID = newWireID()
	}
	body, err := json.Marshal(frame)
	if err != nil {
		fmt.Fprintf(env.stderr, "htmlclay wire send: %v\n", err)
		return wireExitUsage
	}

	cands := wireResolveCandidates(env, file, *port)
	if *port == 0 {
		if err := wireAmbiguity(env.ctx, cands, file); err != nil {
			return wireReport(env, file, err)
		}
	}
	resp, _, err := wireTry(cands, "application/json", func(c wireCandidate) (*http.Response, error) {
		return wirePost(env.ctx, c, body)
	})
	if err != nil {
		if env.ctx.Err() != nil {
			return wireExitOK
		}
		return wireReport(env, file, err)
	}
	defer resp.Body.Close()

	reply, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	fmt.Fprintf(env.stdout, "%s\n", bytes.TrimSpace(reply))
	if resp.StatusCode != http.StatusOK {
		return wireStatusExit(resp.StatusCode)
	}

	var ack struct {
		Delivered int `json:"delivered"`
	}
	if err := json.Unmarshal(reply, &ack); err != nil {
		return wireExitRefused
	}
	if ack.Delivered == 0 {
		// The frame was accepted and nobody was there to take it. Saying so with
		// an exit code is the difference between a script that notices and one
		// that reports success into an empty room.
		fmt.Fprintf(env.stderr, "htmlclay wire send: no handler is attached to %s\n", file)
		return wireExitNotDelivered
	}
	return wireExitOK
}

// wireReadPayload reads the frame's payload from stdin. A terminal is left
// alone: `wire send x --type wire/ping` from a shell must not hang waiting for
// an EOF the user did not know to give.
//
// The read runs on its own goroutine so Ctrl-C still works. A pipe whose writer
// is alive but silent (a supervisor, a FIFO) otherwise blocks here forever,
// before a single request has been made.
func wireReadPayload(env *wireEnv) (json.RawMessage, error) {
	if env.stdin == nil {
		return nil, nil
	}
	if f, ok := env.stdin.(*os.File); ok {
		info, err := f.Stat()
		if err != nil || info.Mode()&os.ModeCharDevice != 0 {
			return nil, nil
		}
	}

	type result struct {
		raw []byte
		err error
	}
	done := make(chan result, 1)
	go func() {
		raw, err := io.ReadAll(io.LimitReader(env.stdin, wireMaxFrame))
		done <- result{raw, err}
	}()

	var got result
	select {
	case got = <-done:
	case <-env.ctx.Done():
		return nil, errors.New("interrupted while reading the payload from stdin")
	}
	if got.err != nil {
		return nil, fmt.Errorf("cannot read the payload from stdin: %w", got.err)
	}
	raw := bytes.TrimSpace(got.raw)
	if len(raw) == 0 {
		return nil, nil
	}
	if !json.Valid(raw) {
		return nil, errors.New("the payload on stdin is not valid JSON")
	}
	return json.RawMessage(raw), nil
}

// --- serve -----------------------------------------------------------------

// wireServer runs one child process per request. It is the product surface: a
// handler is whatever the user typed after --, so `claude -p "$(cat)"` is a
// working agent with no code.
type wireServer struct {
	env      *wireEnv
	ctx      context.Context
	file     string
	cmd      []string
	protocol string
	sender   *wireSender

	mu   sync.Mutex
	live map[string]context.CancelFunc
	wg   sync.WaitGroup
}

func wireServeCmd(env *wireEnv, args []string) int {
	sep := -1
	for i, a := range args {
		if a == "--" {
			sep = i
			break
		}
	}
	if sep < 0 || sep == len(args)-1 {
		fmt.Fprint(env.stderr, wireUsage)
		return wireExitUsage
	}

	fs := wireFlagSet(env, "serve")
	port := fs.Int("port", 0, "the origin's loopback port, instead of looking one up")
	protocol := fs.String("protocol", "raw", "helper output protocol: raw or jsonl")
	operands, err := wireParse(fs, args[:sep])
	if err != nil {
		return wireExitUsage
	}
	if *protocol != "raw" && *protocol != "jsonl" {
		fmt.Fprintf(env.stderr, "htmlclay wire: unsupported helper protocol %q\n", *protocol)
		return wireExitUsage
	}
	file, code := wireFileArg(env, operands)
	if code != 0 {
		return code
	}

	sv := &wireServer{
		env:      env,
		ctx:      env.ctx,
		file:     file,
		cmd:      args[sep+1:],
		protocol: *protocol,
		sender:   &wireSender{env: env, file: file, port: *port},
		live:     make(map[string]context.CancelFunc),
	}
	exit := wireStream(env.ctx, env, wireStreamOpts{
		file: file, port: *port, handler: true, mode: *protocol, onFrame: sv.onFrame,
	})
	sv.stopAll()
	return exit
}

// onFrame acts on the two inbound types and ignores the rest, which is also how
// it ignores its own echoes: everything this process sends is outbound.
//
// It runs on the goroutine reading the stream, so it must never block on the
// network. Every send it could make is handed to a goroutine for that reason: a
// reader stalled behind an HTTP post is a reader not draining its subscription,
// and the server evicts a subscriber whose queue fills.
func (sv *wireServer) onFrame(raw []byte, f wireFrame) {
	switch f.Type {
	case "wire/request":
		sv.start(raw, f)
	case "wire/describe":
		if sv.protocol == "jsonl" {
			sv.start(raw, f)
		}
	case "wire/cancel":
		sv.cancel(f.ID)
	}
}

func (sv *wireServer) start(raw []byte, f wireFrame) {
	if f.ID == "" {
		return
	}
	var ctx context.Context
	var cancel context.CancelFunc
	if sv.protocol == "jsonl" {
		budgetMS, _ := structuredRequestLimits(f.Type)
		ctx, cancel = context.WithTimeout(sv.ctx, time.Duration(budgetMS)*time.Millisecond)
	} else {
		ctx, cancel = context.WithCancel(sv.ctx)
	}

	sv.mu.Lock()
	if _, running := sv.live[f.ID]; running {
		sv.mu.Unlock()
		cancel()
		// Not the silent drop hyper-wire's serve.js does: ids are opaque uuids, so
		// a repeat is this process seeing one request twice (a replayed frame, a
		// resend), never two different requests colliding.
		sv.say(f.ID, "already running; ignoring the repeat")
		return
	}
	if len(sv.live) >= wireServeMax {
		sv.mu.Unlock()
		cancel()
		full := fmt.Sprintf("this handler is already running %d requests", wireServeMax)
		go func() {
			sv.sender.send(wireFrame{Type: "wire/error", ID: f.ID, File: sv.file, Text: full})
			sv.say(f.ID, "refused: "+full)
		}()
		return
	}
	sv.live[f.ID] = cancel
	sv.wg.Add(1)
	sv.mu.Unlock()

	go sv.run(ctx, raw, f.ID, f.Type)
}

func (sv *wireServer) run(ctx context.Context, raw []byte, id, requestType string) {
	defer sv.wg.Done()
	defer func() {
		sv.mu.Lock()
		if cancel, ok := sv.live[id]; ok {
			delete(sv.live, id)
			cancel()
		}
		sv.mu.Unlock()
	}()
	if sv.protocol == "jsonl" {
		sv.runStructuredRequest(ctx, raw, id, requestType)
		return
	}

	// The ack rides this goroutine rather than the stream reader's, so picking a
	// request up can never stall the subscription that delivers the next one.
	sv.sender.send(wireFrame{Type: "wire/ack", ID: id, File: sv.file})

	stderr := &wirePrefixWriter{w: sv.env.stderr, prefix: "[" + wireShortID(id) + "] "}
	type queuedEvent struct {
		event HelperEvent
		sent  chan struct{}
	}
	events := make(chan queuedEvent, 16)
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		for queued := range events {
			event := queued.event
			frame := wireFrame{ID: id, File: sv.file, Text: event.Text}
			switch event.Kind {
			case "status":
				frame.Type = "wire/status"
			case "result":
				frame.Type = "wire/done"
			case "error":
				frame.Type = "wire/error"
			}
			sv.sender.send(frame)
			if queued.sent != nil {
				close(queued.sent)
			}
		}
	}()

	Run(ctx, HelperSpec{
		Argv:   sv.cmd,
		Env:    append(os.Environ(), "HTMLCLAY_WIRE_FILE="+sv.file, "HTMLCLAY_WIRE_ID="+id),
		Stdin:  raw,
		Stderr: stderr,
	}, func(event HelperEvent) {
		queued := queuedEvent{event: event}
		if event.Kind == "result" || event.Kind == "error" {
			queued.sent = make(chan struct{})
		}
		events <- queued
		if queued.sent != nil {
			<-queued.sent
		}
	})
	close(events)
	<-drained
}

func (sv *wireServer) runStructuredRequest(ctx context.Context, raw []byte, id, requestType string) {
	budgetMS, resultLimit := structuredRequestLimits(requestType)
	ack := wireFrame{
		Type: "wire/ack", ID: id, File: sv.file,
		Payload: rawJSON(struct {
			Mode     string `json:"mode"`
			BudgetMS int    `json:"budgetMs"`
		}{Mode: "jsonl", BudgetMS: budgetMS}),
	}
	if err := sv.sender.sendStructured(ack); err != nil {
		fmt.Fprintf(sv.env.stderr, "[wire] cannot encode wire/ack: %v\n", err)
	}

	input, err := stampHelperProtocol(raw)
	if err != nil {
		event := structuredHostError("helper_bad_output", "cannot prepare the helper request", helperFailureDetails("request", err.Error()))
		frame, body, frameErr := encodeHelperEventFrame(id, sv.file, event, resultLimit)
		if frameErr == nil {
			sv.sender.sendBody(frame, body)
		}
		return
	}

	stderr := &wirePrefixWriter{w: sv.env.stderr, prefix: "[" + wireShortID(id) + "] "}
	queue := newStructuredEventQueue(func(event HelperEvent) {
		frame, body, err := encodeHelperEventFrame(id, sv.file, event, resultLimit)
		if err != nil {
			fmt.Fprintf(sv.env.stderr, "[wire] cannot encode helper outcome for %s: %v\n", wireShortID(id), err)
			return
		}
		sv.sender.sendBody(frame, body)
	})
	Run(ctx, HelperSpec{
		Argv:       sv.cmd,
		Dir:        filepath.Dir(sv.file),
		Env:        append(os.Environ(), "HTMLCLAY_WIRE_FILE="+sv.file, "HTMLCLAY_WIRE_ID="+id),
		Deadline:   time.Duration(budgetMS) * time.Millisecond,
		Structured: true,
		Stdin:      input,
		Stderr:     stderr,
	}, queue.emit)
}

func structuredRequestLimits(requestType string) (budgetMS, resultLimit int) {
	if requestType == "wire/describe" {
		return helperDescribeDeadline, helperDescribeResultLimit
	}
	return helperStructuredDeadline, helperMaxTerminalRecord
}

// say reports one request's outcome on the CLI's own stderr, in the same
// bracketed form the handler's stderr is prefixed with.
func (sv *wireServer) say(id, text string) {
	fmt.Fprintf(sv.env.stderr, "[%s] %s\n", wireShortID(id), text)
}

// wireReadLine returns one line, truncated to max bytes, discarding the rest of
// an overlong one rather than stopping.
//
// A bufio.Scanner cannot do this: it fails the whole stream with ErrTooLong on a
// line past its buffer, and a reader that stops reading is a child blocked
// forever writing into a full pipe, with no terminal frame and a request slot
// held until something cancels it. A handler printing one very long line (a JSON
// result, a minified document) is ordinary, so it must cost a truncated status
// and nothing more.
func wireReadLine(r *bufio.Reader, max int) (string, error) {
	var b strings.Builder
	for {
		chunk, err := r.ReadSlice('\n')
		if room := max - b.Len(); room > 0 {
			if len(chunk) > room {
				chunk = chunk[:room]
			}
			b.Write(chunk)
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue // still inside one long line; keep discarding it
		}
		return strings.TrimRight(b.String(), "\r\n"), err
	}
}

func (sv *wireServer) cancel(id string) {
	sv.mu.Lock()
	cancel, ok := sv.live[id]
	sv.mu.Unlock()
	if !ok {
		return
	}
	fmt.Fprintf(sv.env.stderr, "[wire] cancelling %s\n", id)
	cancel()
}

// stopAll ends every request and WAITS for it. Cancellation is asynchronous: the
// signal is delivered by exec's own watchdog goroutine, so returning here without
// waiting lets the CLI exit while a handler it started is still running and still
// writing the file, with nothing watching it.
func (sv *wireServer) stopAll() {
	sv.mu.Lock()
	cancels := make([]context.CancelFunc, 0, len(sv.live))
	for _, cancel := range sv.live {
		cancels = append(cancels, cancel)
	}
	sv.mu.Unlock()
	for _, cancel := range cancels {
		cancel()
	}

	done := make(chan struct{})
	go func() {
		sv.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(cmdStopBudget):
		fmt.Fprintf(sv.env.stderr, "[wire] a handler did not exit in time; leaving it running\n")
	}
}

// cmdStopBudget covers exec's own WaitDelay (5s) plus the drain window, so the
// only way to hit it is a handler that ignored both.
const cmdStopBudget = 8 * time.Second

func wireShortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

// wirePrefixWriter tags a child's stderr with its request id, so several handlers
// running at once produce a readable log instead of interleaved fragments.
type wirePrefixWriter struct {
	w      io.Writer
	prefix string
	mu     sync.Mutex
	buf    bytes.Buffer
}

// wirePrefixMax bounds the partial line held while waiting for a newline. A
// child that writes without ever ending a line would otherwise grow this without
// limit.
const wirePrefixMax = 64 << 10

func (p *wirePrefixWriter) Write(b []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.buf.Write(b)
	for {
		line, err := p.buf.ReadString('\n')
		if err != nil {
			p.buf.WriteString(line)
			break
		}
		fmt.Fprintf(p.w, "%s%s", p.prefix, line)
	}
	if p.buf.Len() > wirePrefixMax {
		fmt.Fprintf(p.w, "%s%s\n", p.prefix, p.buf.String())
		p.buf.Reset()
	}
	return len(b), nil
}

// Flush emits whatever the child wrote without a trailing newline, so a one-line
// diagnostic from a handler that died mid-sentence is not swallowed.
func (p *wirePrefixWriter) Flush() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.buf.Len() == 0 {
		return
	}
	fmt.Fprintf(p.w, "%s%s\n", p.prefix, p.buf.String())
	p.buf.Reset()
}

// wireSender posts frames back to whichever origin currently owns the file.
//
// The candidate list is remembered between sends and re-read on a miss. That is
// not the cached origin the reconnect rule forbids: a stale port here costs one
// 404 and a re-read, where a stale port on the stream would silently attach to
// the wrong listener.
//
// Its posts deliberately do NOT ride the signal context. Ctrl-C cancels every
// request, and each one's terminal frame is sent after that: on the signal
// context those posts would all fail and the page would be left with requests
// that never ended. The client's own timeout is what bounds them instead.
type wireSender struct {
	env   *wireEnv
	file  string
	port  int
	mu    sync.Mutex
	cands []wireCandidate
}

func (s *wireSender) send(f wireFrame) {
	body, err := json.Marshal(f)
	if err != nil {
		fmt.Fprintf(s.env.stderr, "[wire] cannot encode %s: %v\n", f.Type, err)
		return
	}
	s.sendBody(f, body)
}

func (s *wireSender) sendStructured(f wireFrame) error {
	body, err := encodeFrame(f)
	if err != nil {
		return err
	}
	if len(body) > helperMaxWireEnvelope {
		return fmt.Errorf("encoded frame is %d bytes, limit is %d", len(body), helperMaxWireEnvelope)
	}
	s.sendBody(f, body)
	return nil
}

func (s *wireSender) sendBody(f wireFrame, body []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for pass := 0; pass < 2; pass++ {
		if pass == 1 || s.cands == nil {
			s.cands = wireResolveCandidates(s.env, s.file, s.port)
		}
		resp, _, tErr := wireTry(s.cands, "application/json", func(c wireCandidate) (*http.Response, error) {
			return wirePost(context.Background(), c, body)
		})
		if tErr != nil {
			continue
		}
		if resp.StatusCode != http.StatusOK {
			fmt.Fprintf(s.env.stderr, "[wire] %s for %s refused: %s\n", f.Type, wireShortID(f.ID), resp.Status)
		}
		wireDrain(resp)
		return
	}
	fmt.Fprintf(s.env.stderr, "[wire] could not deliver %s for %s: no origin is serving %s\n",
		f.Type, wireShortID(f.ID), s.file)
}

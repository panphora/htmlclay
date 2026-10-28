package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/panphora/htmlclay/internal/aiedit"
	"github.com/panphora/htmlclay/internal/config"
	"github.com/panphora/htmlclay/internal/helper"
	"github.com/panphora/htmlclay/internal/htmlutil"
	"github.com/panphora/htmlclay/internal/session"
)

const (
	helperServeMax  = 8
	helperScanLimit = 512 << 10
)

var errHelperHandlerTaken = errors.New("an external wire handler is already attached")

type helperMeta struct {
	Name  string `json:"name"`
	State string `json:"state"`
}

type helperDispatcher struct {
	server     *Server
	file       string
	names      []string
	declared   map[string]bool
	allowed    map[string]config.HelperProgram
	denied     map[string]bool
	sub        *wireSub
	generation uint64
	ctx        context.Context
	cancel     context.CancelFunc
	mu         sync.Mutex
	live       map[string]liveHelperRun
}

type liveHelperRun struct {
	cancel  context.CancelFunc
	helper  string
	program string
}

// AttachHelpers installs one in-process structured dispatcher for file. Calling
// it again with the same declaration and allow set reuses the existing handler.
//
// denied lists the declared names the user actually refused. It is not the
// complement of allowed: a name the user was never asked about, or whose
// program has since been unregistered, is in neither, and discovery has to be
// able to tell those apart from a refusal.
func (s *Server) AttachHelpers(file string, allowed map[string]config.HelperProgram, denied []string) error {
	names, err := readDeclaredHelpers(file)
	if err != nil {
		return fmt.Errorf("read helper declarations for %s: %w", file, err)
	}
	if len(names) == 0 {
		s.DetachHelpers(file)
		return nil
	}
	declared := make(map[string]bool, len(names))
	for _, name := range names {
		declared[name] = true
	}
	allowCopy := make(map[string]config.HelperProgram, len(allowed))
	for name, program := range allowed {
		if declared[name] && config.ValidHelperName(name) && program.ID != "" {
			allowCopy[name] = program
		}
	}
	denyCopy := make(map[string]bool, len(denied))
	for _, name := range denied {
		if declared[name] && allowCopy[name].ID == "" {
			denyCopy[name] = true
		}
	}

	s.helperMu.Lock()
	defer s.helperMu.Unlock()
	if current := s.helpers[file]; current != nil {
		if reflect.DeepEqual(current.names, names) {
			if !reflect.DeepEqual(current.allowed, allowCopy) || !reflect.DeepEqual(current.denied, denyCopy) {
				current.regrantLocked(allowCopy, denyCopy)
			}
			return nil
		}
		s.detachHelperLocked(current, false)
	}
	// The remembered conflict is not asked whether it still holds. An external
	// handler releases the slot when its terminal quits, and the next open has
	// to be able to take it. s.wire.add below is the only authority on whether
	// the slot is free, and it restores this entry when it is not.
	delete(s.helperOffline, file)

	ctx, cancel := context.WithCancel(context.Background())
	d := &helperDispatcher{
		server:   s,
		file:     file,
		names:    append([]string(nil), names...),
		declared: declared,
		allowed:  allowCopy,
		denied:   denyCopy,
		ctx:      ctx,
		cancel:   cancel,
		live:     make(map[string]liveHelperRun),
	}
	d.sub = &wireSub{
		key:     file,
		handler: true,
		mode:    "jsonl",
		ch:      make(chan []byte, wireQueueSize),
		done:    make(chan struct{}),
	}
	if _, _, err := s.wire.add(d.sub, 0); err != nil {
		cancel()
		if s.helperOffline == nil {
			s.helperOffline = make(map[string][]string)
		}
		s.helperOffline[file] = append([]string(nil), names...)
		if errors.Is(err, errWireHandlerTaken) {
			return errHelperHandlerTaken
		}
		return err
	}
	if s.helpers == nil {
		s.helpers = make(map[string]*helperDispatcher)
	}
	d.generation = s.BindHelpers(file)
	s.helpers[file] = d
	go d.loop()
	return nil
}

// DetachHelpers revokes file's dispatcher and cancels all accepted work.
func (s *Server) DetachHelpers(file string) {
	s.helperMu.Lock()
	defer s.helperMu.Unlock()
	delete(s.helperOffline, file)
	if d := s.helpers[file]; d != nil {
		s.detachHelperLocked(d, false)
	}
}

// regrantLocked replaces what the dispatcher allows without replacing it, so a
// run that is still permitted keeps its subscriber and its terminal outcome. A
// run whose helper lost its grant, or was regranted to a different program, is
// cancelled and publishes its own terminal through the same dispatcher.
// grantedLocked reports whether name is still allowed with programID. Caller
// holds server.helperMu.
func (d *helperDispatcher) grantedLocked(name, programID string) bool {
	current, ok := d.allowed[name]
	return ok && current.ID == programID
}

func (d *helperDispatcher) regrantLocked(allowed map[string]config.HelperProgram, denied map[string]bool) {
	d.allowed = allowed
	d.denied = denied
	var cancels []context.CancelFunc
	d.mu.Lock()
	for _, run := range d.live {
		program, ok := allowed[run.helper]
		if !ok || program.ID != run.program {
			cancels = append(cancels, run.cancel)
		}
	}
	d.mu.Unlock()
	for _, cancel := range cancels {
		cancel()
	}
}

func (s *Server) detachHelperLocked(d *helperDispatcher, unavailable bool) {
	if s.helpers[d.file] != d {
		return
	}
	delete(s.helpers, d.file)
	if unavailable {
		if s.helperOffline == nil {
			s.helperOffline = make(map[string][]string)
		}
		s.helperOffline[d.file] = append([]string(nil), d.names...)
	}
	s.UnbindHelpers(d.file)
	d.stop()
}

func readDeclaredHelpers(file string) ([]string, error) {
	f, err := os.Open(file)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, helperScanLimit))
	if err != nil {
		return nil, err
	}
	return htmlutil.ReadHelperNames(data), nil
}

func (d *helperDispatcher) stop() {
	d.cancel()
	d.mu.Lock()
	cancels := make([]context.CancelFunc, 0, len(d.live))
	for _, run := range d.live {
		cancels = append(cancels, run.cancel)
	}
	d.mu.Unlock()
	for _, cancel := range cancels {
		cancel()
	}
}

func (d *helperDispatcher) loop() {
	defer func() {
		d.server.helperMu.Lock()
		d.server.detachHelperLocked(d, true)
		d.server.helperMu.Unlock()
		d.server.wire.remove(d.sub)
	}()
	for {
		select {
		case <-d.ctx.Done():
			return
		case <-d.server.wire.closing:
			return
		case <-d.sub.done:
			return
		case raw := <-d.sub.ch:
			env, err := decodeWireEnvelope(raw)
			if err != nil {
				d.server.logger.Printf("helper dispatcher for %s received an invalid frame: %v", d.file, err)
				continue
			}
			switch env.Type {
			case "wire/request", "wire/describe":
				d.start(env, raw)
			case "wire/cancel":
				d.cancelRequest(env.ID)
			}
		}
	}
}

func decodeWireEnvelope(framed []byte) (wireEnvelope, error) {
	var data []string
	for _, line := range strings.Split(string(framed), "\n") {
		if strings.HasPrefix(line, "data:") {
			data = append(data, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		}
	}
	if len(data) == 0 {
		return wireEnvelope{}, errors.New("SSE frame has no data line")
	}
	var env wireEnvelope
	if err := json.Unmarshal([]byte(strings.Join(data, "\n")), &env); err != nil {
		return wireEnvelope{}, err
	}
	return env, nil
}

func (d *helperDispatcher) start(env wireEnvelope, framed []byte) {
	if env.ID == "" || len(env.ID) > maxWireIDLen {
		return
	}
	if env.Helper == "" {
		d.reject(env, "helper_name_required", "a helper name is required")
		return
	}
	if !config.ValidHelperName(env.Helper) {
		d.reject(env, "invalid_helper", "the helper name is invalid")
		return
	}
	if !d.declared[env.Helper] {
		d.reject(env, "helper_not_declared", "this document did not declare that helper")
		return
	}
	document := env.Document
	if document == "" {
		document = "none"
	}
	if document != "none" && document != "edit" {
		d.reject(env, "invalid_document", `document must be "edit" or "none"`)
		return
	}
	d.server.helperMu.Lock()
	program, ok := d.allowed[env.Helper]
	refused := d.denied[env.Helper]
	d.server.helperMu.Unlock()
	if !ok {
		if env.Type == "wire/request" && !refused && d.server.hooks.AskHelper != nil {
			d.askThenStart(env, framed)
			return
		}
		d.reject(env, "helper_not_granted", "this document was not granted that helper")
		return
	}

	d.server.helperMu.Lock()
	if !d.server.helperCurrentLocked(d) {
		d.server.helperMu.Unlock()
		return
	}
	// A regrant can land between the read above and this registration, and it
	// keeps the generation, so the grant itself is checked again here and in Start.
	if !d.grantedLocked(env.Helper, program.ID) {
		d.server.helperMu.Unlock()
		d.reject(env, "helper_not_granted", "this document was not granted that helper")
		return
	}
	budgetMS, resultLimit := helperRequestLimits(env.Type)
	ctx, cancel := context.WithTimeout(d.ctx, time.Duration(budgetMS)*time.Millisecond)
	d.mu.Lock()
	if _, exists := d.live[env.ID]; exists {
		d.mu.Unlock()
		d.server.helperMu.Unlock()
		cancel()
		d.reject(env, "duplicate_request", "a request with this id is already running")
		// This refusal is about the second request, not about the outcome of the
		// first, which is still running and will publish its own terminal. Left
		// retained it would win the id's one remembered outcome under the
		// first-terminal-wins rule, and a subscriber that reconnects after the
		// real answer landed would replay this error instead.
		d.server.wire.forgetTerminal(d.file, env.ID)
		return
	}
	if len(d.live) >= helperServeMax {
		d.mu.Unlock()
		d.server.helperMu.Unlock()
		cancel()
		d.reject(env, "helper_busy", fmt.Sprintf("this handler is already running %d requests", helperServeMax))
		return
	}
	d.live[env.ID] = liveHelperRun{cancel: cancel, helper: env.Helper, program: program.ID}
	d.mu.Unlock()
	d.server.wire.forgetTerminal(d.file, env.ID)
	d.server.helperMu.Unlock()

	ack := wireEnvelope{
		V:      1,
		Type:   "wire/ack",
		ID:     env.ID,
		From:   "process",
		File:   d.file,
		Helper: env.Helper,
		Payload: encodeHelperJSON(struct {
			Mode     string `json:"mode"`
			BudgetMS int    `json:"budgetMs"`
		}{Mode: "jsonl", BudgetMS: budgetMS}),
	}
	d.publish(ack, false)
	go d.run(ctx, cancel, env, framed, program, document, resultLimit)
}

// helperAskBudget covers the whole approval: the dialog gives up after 120
// seconds, the file picker after 5 minutes, and the program check can ask once more.
// The parked ack adds the run's own deadline to it, because approving reattaches
// the helpers, which drops the page's stream: the run's ack can land while the
// page is reconnecting, and only terminal frames are replayed.
const helperAskBudget = 10 * time.Minute

// helperAskQuietFor is how long a page must wait to raise the dialog again
// after one that granted nothing. A Deny needs no timer: it marks the name
// denied for this open, and start refuses it without asking.
const helperAskQuietFor = time.Minute

type parkedHelperRequest struct {
	env    wireEnvelope
	framed []byte
}

func helperAskKey(file, name string) string { return file + "\x00" + name }

// askThenStart parks a request for a helper the person has not answered and
// has the app raise the dialog, once per file and name however many requests
// arrive meanwhile. The page gets an ack whose budget covers the dialog and the
// picker, and a status saying what it waits on.
func (d *helperDispatcher) askThenStart(env wireEnvelope, framed []byte) {
	s := d.server
	key := helperAskKey(d.file, env.Helper)
	s.helperMu.Lock()
	if !s.helperCurrentLocked(d) {
		s.helperMu.Unlock()
		return
	}
	if time.Now().Before(s.helperAskQuiet[d.file]) {
		s.helperMu.Unlock()
		d.reject(env, "helper_not_granted", "this document was not granted that helper")
		return
	}
	parked, asking := s.helperAsking[key]
	if len(parked) >= helperServeMax {
		s.helperMu.Unlock()
		d.reject(env, "helper_busy", fmt.Sprintf("this handler is already running %d requests", helperServeMax))
		return
	}
	if s.helperAsking == nil {
		s.helperAsking = make(map[string][]parkedHelperRequest)
	}
	s.helperAsking[key] = append(parked, parkedHelperRequest{env: env, framed: framed})
	s.helperMu.Unlock()

	d.publish(wireEnvelope{
		V:      1,
		Type:   "wire/ack",
		ID:     env.ID,
		From:   "process",
		File:   d.file,
		Helper: env.Helper,
		Payload: encodeHelperJSON(struct {
			Mode     string `json:"mode"`
			BudgetMS int    `json:"budgetMs"`
		}{Mode: "jsonl", BudgetMS: int(helperAskBudget/time.Millisecond) + helper.StructuredDeadline}),
	}, false)
	d.publishEvent(env, helper.Event{Kind: "status", Text: "Waiting for you to allow " + env.Helper + " in HTML Clay"}, false, helper.MaxTerminalRecord)
	if !asking {
		go s.finishHelperAsk(d.file, env.Helper)
	}
}

// finishHelperAsk waits for the dialog, then starts every parked request on the
// dispatcher the approval attached, or refuses each one. It refuses rather than
// re-dispatching when the current dispatcher does not hold the helper, so a
// failed reattach can never loop back into another dialog.
func (s *Server) finishHelperAsk(file, name string) {
	allowed := s.hooks.AskHelper(file, name)
	key := helperAskKey(file, name)
	s.helperMu.Lock()
	parked := s.helperAsking[key]
	delete(s.helperAsking, key)
	if !allowed {
		if s.helperAskQuiet == nil {
			s.helperAskQuiet = make(map[string]time.Time)
		}
		s.helperAskQuiet[file] = time.Now().Add(helperAskQuietFor)
	}
	current := s.helpers[file]
	granted := current != nil && current.allowed[name].ID != ""
	s.helperMu.Unlock()
	if current == nil {
		for _, p := range parked {
			s.wire.publish(file, helperEventEnvelope(p.env, helper.Event{Kind: "error", Text: "this document was not granted that helper", Code: "helper_not_granted", Source: "host"}, helper.MaxTerminalRecord))
		}
		return
	}
	for _, p := range parked {
		if allowed && granted {
			current.start(p.env, p.framed)
		} else {
			current.reject(p.env, "helper_not_granted", "this document was not granted that helper")
		}
	}
}

func (s *Server) helperCurrentLocked(d *helperDispatcher) bool {
	return s.helpers[d.file] == d && s.HelperGeneration(d.file) == d.generation
}

func (d *helperDispatcher) reject(env wireEnvelope, code, message string) {
	d.publishEvent(env, helper.Event{Kind: "error", Text: message, Code: code, Source: "host"}, false, helper.MaxTerminalRecord)
}

func (d *helperDispatcher) cancelRequest(id string) {
	s := d.server
	s.helperMu.Lock()
	prefix := d.file + "\x00"
	for key, parked := range s.helperAsking {
		if !strings.HasPrefix(key, prefix) {
			continue
		}
		s.helperAsking[key] = slices.DeleteFunc(parked, func(p parkedHelperRequest) bool { return p.env.ID == id })
	}
	s.helperMu.Unlock()
	d.mu.Lock()
	run := d.live[id]
	d.mu.Unlock()
	if run.cancel != nil {
		run.cancel()
	}
}

func (d *helperDispatcher) run(ctx context.Context, cancel context.CancelFunc, env wireEnvelope, framed []byte, program config.HelperProgram, document string, resultLimit int) {
	defer cancel()
	defer func() {
		d.mu.Lock()
		delete(d.live, env.ID)
		d.mu.Unlock()
	}()

	var file *session.File
	if document == "edit" {
		var ok bool
		file, ok = d.server.sessions.LookupByPath(d.file)
		if !ok {
			d.reject(env, "helper_start_failed", "the document is no longer registered")
			return
		}
		if err := d.server.openHistoryForHandler(file); err != nil {
			d.reject(env, "helper_start_failed", "could not prepare document history: "+err.Error())
			return
		}
		d.server.coord.lease(file)
		defer d.server.coord.unlease(file)
	}

	input, err := helper.StampProtocol(wireData(framed), document)
	if err != nil {
		d.publishEvent(env, helper.Event{Kind: "error", Text: "cannot prepare the helper request", Code: "helper_bad_output", Source: "host"}, document == "edit", resultLimit)
		return
	}
	stderr := &helperLogWriter{server: d.server, id: env.ID, name: env.Helper}
	helper.Run(ctx, helper.Spec{
		Argv:       []string{program.Path},
		Dir:        filepath.Dir(d.file),
		Env:        append(os.Environ(), "HTMLCLAY_WIRE_FILE="+d.file, "HTMLCLAY_WIRE_ID="+env.ID),
		Structured: true,
		LoginPath:  true,
		Stdin:      input,
		Stderr:     stderr,
		Start: func(cmd *exec.Cmd) error {
			d.server.helperMu.Lock()
			defer d.server.helperMu.Unlock()
			if !d.server.helperCurrentLocked(d) || !d.grantedLocked(env.Helper, program.ID) {
				return context.Canceled
			}
			return cmd.Start()
		},
	}, func(event helper.Event) {
		d.publishEvent(env, event, document == "edit", resultLimit)
	})
}

func wireData(framed []byte) []byte {
	var data []string
	for _, line := range strings.Split(string(framed), "\n") {
		if strings.HasPrefix(line, "data:") {
			data = append(data, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		}
	}
	return []byte(strings.Join(data, "\n"))
}

func helperRequestLimits(requestType string) (budgetMS, resultLimit int) {
	if requestType == "wire/describe" {
		return helper.DescribeDeadline, helper.DescribeResultLimit
	}
	return helper.StructuredDeadline, helper.MaxTerminalRecord
}

func (d *helperDispatcher) publishEvent(request wireEnvelope, event helper.Event, editing bool, resultLimit int) {
	env := helperEventEnvelope(request, event, resultLimit)
	if encodedHelperEnvelopeSize(env) > helper.MaxWireEnvelope {
		env = helperEventEnvelope(request, helper.Event{
			Kind:   "error",
			Text:   "helper result does not fit in a wire envelope",
			Code:   "helper_result_too_large",
			Source: "host",
		}, resultLimit)
	}
	d.publish(env, editing && env.isTerminal())
}

func (d *helperDispatcher) publish(env wireEnvelope, poke bool) {
	d.server.helperMu.Lock()
	defer d.server.helperMu.Unlock()
	if !d.server.helperCurrentLocked(d) {
		return
	}
	_, _, published := d.server.wire.publishFromHandler(d.file, d.sub, env)
	if !published {
		return
	}
	if poke {
		if file, ok := d.server.sessions.LookupByPath(d.file); ok {
			d.server.coord.pokeWatcher(file)
		}
	}
}

func helperEventEnvelope(request wireEnvelope, event helper.Event, resultLimit int) wireEnvelope {
	if event.Kind == "result" && resultLimit > 0 && len(event.Value) > resultLimit {
		event = helper.Event{Kind: "error", Text: "helper result exceeds its byte limit", Code: "helper_result_too_large", Source: "host"}
	}
	env := wireEnvelope{V: 1, ID: request.ID, From: "process", File: request.File, Helper: request.Helper, Text: event.Text}
	switch event.Kind {
	case "status":
		env.Type = "wire/status"
		if event.Progress != nil {
			env.Payload = encodeHelperJSON(struct {
				Progress json.RawMessage `json:"progress"`
			}{Progress: event.Progress})
		}
	case "result":
		env.Type = "wire/done"
		env.Payload = event.Value
	case "error":
		env.Type = "wire/error"
		source := event.Source
		if source == "" {
			source = "host"
		}
		env.Payload = encodeHelperJSON(struct {
			Source  string          `json:"source"`
			Code    string          `json:"code"`
			Details json.RawMessage `json:"details,omitempty"`
		}{Source: source, Code: event.Code, Details: event.Details})
	}
	return env
}

func encodeHelperJSON(value any) json.RawMessage {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(value); err != nil {
		return nil
	}
	return json.RawMessage(bytes.TrimSuffix(buf.Bytes(), []byte("\n")))
}

func encodedHelperEnvelopeSize(env wireEnvelope) int {
	return len(encodeHelperJSON(env))
}

// helperDiscovery is the document's declared helpers, then the built-in ai-edit
// when this server offers it.
func (s *Server) helperDiscovery(file string) (string, []helperMeta) {
	mode, meta := s.declaredHelperDiscovery(file)
	if entry, ok := s.aiEditMeta(file); ok {
		meta = append(meta, entry)
	}
	return mode, meta
}

func (s *Server) declaredHelperDiscovery(file string) (string, []helperMeta) {
	s.helperMu.Lock()
	var names []string
	var allowed map[string]config.HelperProgram
	var denied map[string]bool
	if d := s.helpers[file]; d != nil {
		names = append([]string(nil), d.names...)
		allowed = make(map[string]config.HelperProgram, len(d.allowed))
		for name, program := range d.allowed {
			allowed[name] = program
		}
		denied = make(map[string]bool, len(d.denied))
		for name := range d.denied {
			denied[name] = true
		}
	} else if offline := s.helperOffline[file]; offline != nil {
		names = append([]string(nil), offline...)
	}
	s.helperMu.Unlock()

	mode, _ := s.wire.handlerMode(file)
	if len(names) == 0 {
		return mode, nil
	}
	meta := make([]helperMeta, 0, len(names))
	for _, name := range names {
		if name == aiedit.HelperName && s.aiEditOffered() {
			continue // the built-in answers this name; a document cannot shadow it
		}
		// unavailable is the honest answer for everything that is not a running
		// program and not a refusal: no dispatcher at all, a name the user was
		// never asked about, a picker the user backed out of, and a program
		// that has since been unregistered or lost its executable bit.
		state := "unavailable"
		switch {
		case denied[name]:
			state = "denied"
		case allowed != nil:
			if program, ok := allowed[name]; ok && helperProgramAvailable(program.Path) {
				state = "ready"
			}
		}
		meta = append(meta, helperMeta{Name: name, State: state})
	}
	return mode, meta
}

func helperProgramAvailable(path string) bool {
	info, err := os.Stat(path)
	if err != nil || info.IsDir() {
		return false
	}
	return runtime.GOOS == "windows" || info.Mode().Perm()&0111 != 0
}

type helperLogWriter struct {
	server *Server
	id     string
	name   string
}

func (w *helperLogWriter) Write(p []byte) (int, error) {
	for _, line := range strings.Split(strings.TrimRight(string(p), "\r\n"), "\n") {
		if line != "" {
			w.server.logger.Printf("helper %s [%s]: %s", w.name, shortHelperID(w.id), line)
		}
	}
	return len(p), nil
}

func shortHelperID(id string) string {
	if len(id) <= 8 {
		return id
	}
	return id[:8]
}

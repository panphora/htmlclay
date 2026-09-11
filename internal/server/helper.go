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
	"strings"
	"sync"
	"time"

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
	live       map[string]context.CancelFunc
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
		if reflect.DeepEqual(current.names, names) && reflect.DeepEqual(current.allowed, allowCopy) && reflect.DeepEqual(current.denied, denyCopy) {
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
		live:     make(map[string]context.CancelFunc),
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
	for _, cancel := range d.live {
		cancels = append(cancels, cancel)
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
	program, ok := d.allowed[env.Helper]
	if !ok {
		d.reject(env, "helper_not_granted", "this document was not granted that helper")
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
	if !d.server.helperCurrentLocked(d) {
		d.server.helperMu.Unlock()
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
	d.live[env.ID] = cancel
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

func (s *Server) helperCurrentLocked(d *helperDispatcher) bool {
	return s.helpers[d.file] == d && s.HelperGeneration(d.file) == d.generation
}

func (d *helperDispatcher) reject(env wireEnvelope, code, message string) {
	d.publishEvent(env, helper.Event{Kind: "error", Text: message, Code: code, Source: "host"}, false, helper.MaxTerminalRecord)
}

func (d *helperDispatcher) cancelRequest(id string) {
	d.mu.Lock()
	cancel := d.live[id]
	d.mu.Unlock()
	if cancel != nil {
		cancel()
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
			if !d.server.helperCurrentLocked(d) {
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

func (s *Server) helperDiscovery(file string) (string, []helperMeta) {
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

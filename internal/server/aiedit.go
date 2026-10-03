package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/panphora/htmlclay/internal/aiedit"
	"github.com/panphora/htmlclay/internal/session"
)

// The built-in ai-edit helper. ClayJS sends a named request with helper
// "ai-edit"; this server answers it itself, from internal/aiedit, with no
// declaration, no approval and without the document's handler slot. Only the
// AI Editing switch gates it, and one request runs per document at a time.

const (
	aiEditBudget        = 5 * time.Minute
	aiEditProgressEvery = 250 * time.Millisecond
	maxAIEditEnvelope   = 1 << 20
	aiEditOffText       = "AI Editing is off. Turn it on in HTML Clay's menu."
)

type aiEditRun struct {
	id     string
	cancel context.CancelFunc
}

type aiEditRegistry struct {
	mu      sync.Mutex
	running map[string]aiEditRun // document path -> its one request in flight
}

func (s *Server) aiEditOffered() bool {
	return s.hooks.AIEditEnabled != nil
}

// aiEditMeta is the discovery entry for a document, when this server offers AI
// editing for it.
func (s *Server) aiEditMeta(file string) (helperMeta, bool) {
	if !s.aiEditOffered() || !aiedit.IsDocument(file) {
		return helperMeta{}, false
	}
	state := "unavailable"
	if s.hooks.AIEditEnabled() {
		state = "ready"
	}
	return helperMeta{Name: aiedit.HelperName, State: state}, true
}

func (s *Server) publishAIEdit(request wireEnvelope, typ, text string, payload any) {
	env := wireEnvelope{
		V:      1,
		Type:   typ,
		ID:     request.ID,
		From:   "process",
		File:   request.File,
		Helper: request.Helper,
		Text:   text,
	}
	if payload != nil {
		env.Payload = encodeHelperJSON(payload)
	}
	if len(encodeHelperJSON(env)) > maxAIEditEnvelope {
		env.Type = "wire/error"
		env.Text = "the rewrite does not fit in a wire envelope"
		env.Payload = encodeHelperJSON(map[string]string{"source": "host", "code": "helper_result_too_large"})
	}
	s.wire.publish(request.File, env)
}

func (s *Server) refuseAIEdit(request wireEnvelope, source, code, text string) {
	s.publishAIEdit(request, "wire/error", text, map[string]string{"source": source, "code": code})
}

// startAIEdit registers the request and runs it in the background. The caller
// has already checked the document's token.
func (s *Server) startAIEdit(f *session.File, request wireEnvelope) {
	if !s.hooks.AIEditEnabled() {
		s.refuseAIEdit(request, "host", "helper_not_granted", aiEditOffText)
		return
	}
	if request.Type == "wire/describe" {
		s.refuseAIEdit(request, "host", "invalid_type", "ai-edit does not support wire/describe")
		return
	}
	var payload aiedit.Payload
	if err := json.Unmarshal(request.Payload, &payload); err != nil {
		s.refuseAIEdit(request, "application", "invalid_request", "malformed ai-edit request")
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), aiEditBudget)
	s.aiEdit.mu.Lock()
	if s.aiEdit.running == nil {
		s.aiEdit.running = make(map[string]aiEditRun)
	}
	if run, busy := s.aiEdit.running[f.AbsPath]; busy {
		s.aiEdit.mu.Unlock()
		cancel()
		if run.id == request.ID {
			s.refuseAIEdit(request, "host", "duplicate_request", "a request with this id is already running")
			// This refusal is about the second request. Left retained, it would win
			// the id's one remembered outcome and a reconnect would replay it
			// instead of the running request's real answer.
			s.wire.forgetTerminal(request.File, request.ID)
		} else {
			s.refuseAIEdit(request, "host", "helper_busy", "an AI edit is already running on this document")
		}
		return
	}
	s.aiEdit.running[f.AbsPath] = aiEditRun{id: request.ID, cancel: cancel}
	s.aiEdit.mu.Unlock()
	// An accepted request starts with no stale outcome under its id.
	s.wire.forgetTerminal(request.File, request.ID)

	s.publishAIEdit(request, "wire/ack", "", map[string]any{"mode": "jsonl", "budgetMs": aiEditBudget.Milliseconds()})
	go s.runAIEdit(ctx, cancel, f.AbsPath, request, payload)
}

func (s *Server) runAIEdit(ctx context.Context, cancel context.CancelFunc, file string, request wireEnvelope, payload aiedit.Payload) {
	// The slot is released before the terminal frame goes out, so a page that
	// sends its next request on seeing that frame is never refused as busy.
	release := func() {
		cancel()
		s.aiEdit.mu.Lock()
		if run, ok := s.aiEdit.running[file]; ok && run.id == request.ID {
			delete(s.aiEdit.running, file)
		}
		s.aiEdit.mu.Unlock()
	}

	var def string
	var engines map[string][]string
	if s.hooks.AIEditEngines != nil {
		def, engines = s.hooks.AIEditEngines()
	}
	opts := aiedit.Options{
		File:     file,
		BaseDir:  filepath.Dir(file),
		Default:  def,
		Engines:  engines,
		Internal: s.isInternal,
		Mock:     os.Getenv("MOCK_MODEL") == "1",
		Status: func(text string) {
			s.publishAIEdit(request, "wire/status", text, nil)
		},
	}

	var progressMu sync.Mutex
	var completed int
	var last time.Time
	progress := func(text string) {
		progressMu.Lock()
		defer progressMu.Unlock()
		completed += len(text)
		now := time.Now()
		if !last.IsZero() && now.Sub(last) < aiEditProgressEvery {
			return
		}
		last = now
		s.publishAIEdit(request, "wire/status", fmt.Sprintf("Writing, %.1f KB", float64(completed)/1024),
			map[string]any{"progress": map[string]any{"completed": completed, "unit": "bytes"}})
	}

	result, err := aiedit.Run(ctx, payload, opts, progress)
	ctxErr := ctx.Err() // read before release, whose cancel() would set it
	release()
	switch {
	case err == nil:
		s.publishAIEdit(request, "wire/done", "", result)
	case errors.Is(ctxErr, context.DeadlineExceeded):
		s.refuseAIEdit(request, "host", "helper_timeout", "the AI edit did not finish within five minutes")
	case ctxErr != nil:
		s.refuseAIEdit(request, "host", "helper_cancelled", "helper cancelled")
	default:
		var coded *aiedit.Error
		if errors.As(err, &coded) {
			s.refuseAIEdit(request, "application", coded.Code, coded.Message)
		} else {
			s.refuseAIEdit(request, "application", "engine_failed", err.Error())
		}
	}
}

// cancelAIEdit cancels the running AI edit on a document when its id matches,
// and reports whether it did.
func (s *Server) cancelAIEdit(file, id string) bool {
	s.aiEdit.mu.Lock()
	defer s.aiEdit.mu.Unlock()
	run, ok := s.aiEdit.running[file]
	if !ok || run.id != id {
		return false
	}
	run.cancel()
	return true
}

// CancelAIEdits cancels every running AI edit: the switch was turned off, or the
// server is shutting down.
func (s *Server) CancelAIEdits() {
	s.aiEdit.mu.Lock()
	defer s.aiEdit.mu.Unlock()
	for _, run := range s.aiEdit.running {
		run.cancel()
	}
}

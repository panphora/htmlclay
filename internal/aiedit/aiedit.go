package aiedit

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode"

	"github.com/panphora/htmlclay/internal/helper"
)

const HelperName = "ai-edit"

const systemPrompt = `You edit one HTML element on a static page.
Reply with exactly one complete element: the revised version of the element you are given, keeping its tag and every attribute it already has (id, class, data-*).
Output raw HTML only, no markdown fences, no commentary before or after. Your reply is morphed into the live page verbatim.
The page's stylesheet is external to the element; stay consistent with the class and structure conventions visible in the element you are given.
Never add <script> tags, inline event handlers or javascript: URLs. The page refuses a reply that contains them.`

type Selection struct {
	Start int `json:"start"`
	End   int `json:"end"`
}

// Payload is what ClayJS sends as the wire request payload.
type Payload struct {
	ID          string     `json:"id"`
	EditID      string     `json:"editId"`
	Tag         string     `json:"tag"`
	ElementHTML string     `json:"elementHTML"`
	Comment     string     `json:"comment"`
	Quote       string     `json:"quote,omitempty"`
	Selection   *Selection `json:"selection,omitempty"`
	ContextRefs []string   `json:"contextRefs,omitempty"`
	Page        bool       `json:"page,omitempty"`
}

type Options struct {
	File    string              // absolute path of the document
	BaseDir string              // folder @file references resolve in (the document's folder)
	Default string              // default engine name; "" means "claude"
	Engines map[string][]string // user-configured engines: name -> argv; "{prompt}" in an arg substitutes the prompt
	Env     []string            // environment for the child; nil means helper.LoginEnv()
	Mock    bool                // deterministic fake reply, for tests and MOCK_MODEL=1
}

type Result struct {
	HTML       string `json:"html"`
	Model      string `json:"model"`
	StopReason string `json:"stopReason"`
}

// Error is a refusal the page shows. Code is one of: invalid_request,
// unknown_engine, invalid_context, engine_unavailable, engine_unsupported,
// engine_failed, declined, incomplete.
type Error struct {
	Code    string
	Message string
}

func (e *Error) Error() string { return e.Message }

// engine is one agent CLI AI editing can start.
type engine struct {
	name    string
	adapter string
	model   string
	command []string
}

var builtinOrder = []string{"claude", "fable", "codex", "agy"}

func builtinEngines() map[string]engine {
	return map[string]engine{
		"claude": {name: "claude", adapter: "claude", model: "claude-opus-5-5"},
		"fable":  {name: "fable", adapter: "claude", model: "claude-fable-5-1"},
		"codex":  {name: "codex", adapter: "codex"},
		"agy":    {name: "agy", adapter: "unsupported"},
	}
}

// A user engine name has to work as a leading @token, and never replaces a
// built-in one.
var userEngineName = regexp.MustCompile(`^[a-z0-9_-]{1,32}$`)

func resolveEngines(configured map[string][]string) map[string]engine {
	engines := builtinEngines()
	for name, command := range configured {
		key := strings.ToLower(name)
		if !userEngineName.MatchString(key) {
			continue
		}
		if _, builtin := engines[key]; builtin {
			continue
		}
		engines[key] = engine{name: key, adapter: "generic", command: command}
	}
	return engines
}

var engineToken = regexp.MustCompile(`^[a-zA-Z0-9_-]+`)

// routeEngine reads a leading bare @word (no dot, no slash) as an engine name.
// @page is a context token, never an engine, and anything else is prose.
func routeEngine(comment string, engines map[string]engine, fallback engine) (engine, string, *Error) {
	trimmed := strings.TrimLeftFunc(comment, unicode.IsSpace)
	if !strings.HasPrefix(trimmed, "@") {
		return fallback, comment, nil
	}
	rest := trimmed[1:]
	word := engineToken.FindString(rest)
	if word == "" || strings.HasPrefix(rest[len(word):], ".") || strings.HasPrefix(rest[len(word):], "/") {
		return fallback, comment, nil
	}
	name := strings.ToLower(word)
	if name == "page" {
		return fallback, comment, nil
	}
	chosen, ok := engines[name]
	if !ok {
		return engine{}, "", unknownEngine(name, engines)
	}
	return chosen, strings.TrimSpace(rest[len(word):]), nil
}

func unknownEngine(name string, engines map[string]engine) *Error {
	seen := make(map[string]bool, len(engines))
	known := make([]string, 0, len(engines))
	for _, builtin := range builtinOrder {
		if _, ok := engines[builtin]; ok {
			known = append(known, "@"+builtin)
			seen[builtin] = true
		}
	}
	extra := make([]string, 0, len(engines))
	for key := range engines {
		if !seen[key] {
			extra = append(extra, "@"+key)
		}
	}
	sort.Strings(extra)
	known = append(known, extra...)
	return &Error{
		Code:    "unknown_engine",
		Message: fmt.Sprintf("unknown agent @%s, this server knows %s", name, strings.Join(known, " ")),
	}
}

func buildUserPrompt(p Payload, comment string, contextSections []string, pageText string) string {
	parts := []string{"The element to edit:\n\n" + p.ElementHTML}
	if p.Quote != "" {
		quote := "The user selected this text inside the element: \"" + p.Quote + "\""
		if p.Selection != nil {
			quote += fmt.Sprintf(" (characters %d to %d of the element's text)", p.Selection.Start, p.Selection.End)
		}
		parts = append(parts, quote)
	}
	parts = append(parts, contextSections...)
	if pageText != "" {
		parts = append(parts, "The full page, for context (@page):\n\n"+pageText)
	}
	parts = append(parts, "Request: "+comment)
	return strings.Join(parts, "\n\n")
}

// Run answers one request. progress receives streamed reply text as it arrives
// (may be nil). A cancelled ctx kills the agent and returns ctx.Err().
func Run(ctx context.Context, p Payload, o Options, progress func(text string)) (Result, error) {
	if p.ElementHTML == "" || p.Comment == "" {
		return Result{}, &Error{Code: "invalid_request", Message: "malformed ai-edit request"}
	}
	engines := resolveEngines(o.Engines)
	defaultName := strings.ToLower(o.Default)
	if defaultName == "" {
		defaultName = "claude"
	}
	fallback, ok := engines[defaultName]
	if !ok {
		return Result{}, &Error{Code: "unknown_engine", Message: fmt.Sprintf("default engine %q is not configured", defaultName)}
	}
	chosen, comment, routeErr := routeEngine(p.Comment, engines, fallback)
	if routeErr != nil {
		return Result{}, routeErr
	}
	if chosen.adapter == "unsupported" {
		return Result{}, &Error{
			Code:    "engine_unsupported",
			Message: fmt.Sprintf("@%s can't be used for AI editing: it can read files without asking, and AI editing only runs agents with no tools", chosen.name),
		}
	}
	report := progress
	if report == nil {
		report = func(string) {}
	}

	pageText := ""
	if p.Page {
		text, err := os.ReadFile(o.File)
		if err != nil {
			return Result{}, &Error{Code: "invalid_request", Message: "cannot read the document"}
		}
		pageText = string(text)
	}
	sections, err := resolveContext(p.ContextRefs, o.BaseDir)
	if err != nil {
		return Result{}, err
	}
	userPrompt := buildUserPrompt(p, comment, sections, pageText)

	label := chosen.model
	if label == "" {
		label = chosen.name
	}
	if o.Mock {
		result, err := mockStream(ctx, p, comment, label, report)
		if err != nil {
			return Result{}, err
		}
		return finish(result)
	}

	program := chosen.name
	switch chosen.adapter {
	case "claude":
		program = "claude"
	case "codex":
		program = "codex"
	case "generic":
		if len(chosen.command) == 0 {
			return Result{}, &Error{Code: "engine_failed", Message: fmt.Sprintf("engine @%s has an empty command", chosen.name)}
		}
		program = chosen.command[0]
	}

	env := o.Env
	if env == nil {
		env = helper.LoginEnv()
	}
	bin, lookErr := lookPath(program, env)
	if lookErr != nil && o.Env == nil {
		env = helper.RefreshLoginEnv()
		bin, lookErr = lookPath(program, env)
	}
	if lookErr != nil {
		missing := "`" + chosen.adapter + "`"
		if chosen.adapter == "generic" {
			missing = "its command"
		}
		return Result{}, &Error{
			Code: "engine_unavailable",
			Message: fmt.Sprintf("@%s isn't available: %s was not found on this machine. Install it and sign in, or pick another agent with @claude, @fable or @codex.",
				chosen.name, missing),
		}
	}

	result, err := adapters[chosen.adapter](ctx, chosen, bin, userPrompt, o, env, report)
	if err != nil {
		return Result{}, err
	}
	return finish(result)
}

func finish(result Result) (Result, error) {
	switch result.StopReason {
	case "refusal":
		return Result{}, &Error{Code: "declined", Message: "the model declined this request"}
	case "end_turn":
		return result, nil
	default:
		return Result{}, &Error{Code: "incomplete", Message: fmt.Sprintf("reply incomplete (%s), not applied", result.StopReason)}
	}
}

// IsDocument reports whether a path is a document AI editing serves: .html or
// .htmlclay, any letter case.
func IsDocument(path string) bool {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".html", ".htmlclay":
		return true
	}
	return false
}

package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"sync"
)

const (
	helperProtocolVersion     = 1
	helperMaxRecord           = 512 << 10
	helperMaxTerminalRecord   = 512 << 10
	helperMaxStatusRecord     = 32 << 10
	helperMaxErrorCode        = 128
	helperMaxProgressUnit     = 64
	helperMaxWireEnvelope     = 1 << 20
	helperStructuredDeadline  = 300000
	helperDescribeDeadline    = 5000
	helperDescribeResultLimit = 8 << 10
)

// The JSON Lines parser lives in internal/helper, which is what the CLI and the
// in-process dispatcher both execute. A second copy lived here until the runner
// moved; it went unreachable while still carrying the grammar tests, so the
// rules were verified against code that never ran.

func encodeFrame(f wireFrame) ([]byte, error) {
	return encodeJSON(f)
}

func encodeJSON(value any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(value); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}

func rawJSON(value any) json.RawMessage {
	raw, err := encodeJSON(value)
	if err != nil {
		return nil
	}
	return json.RawMessage(raw)
}

func stampHelperProtocol(raw []byte) ([]byte, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, err
	}
	if fields == nil {
		return nil, errors.New("request envelope must be an object")
	}
	fields["helperProtocol"] = json.RawMessage(strconv.Itoa(helperProtocolVersion))
	return encodeJSON(fields)
}

func encodeHelperEventFrame(id, file string, event HelperEvent, resultLimit int) (wireFrame, []byte, error) {
	if event.Kind == "result" && resultLimit > 0 && len(event.Value) > resultLimit {
		event = structuredHostError(
			"helper_result_too_large",
			"helper result exceeds its byte limit",
			helperLimitDetails("result", resultLimit),
		)
	}
	frame, err := helperEventFrame(id, file, event)
	if err != nil {
		return wireFrame{}, nil, err
	}
	body, err := encodeFrame(frame)
	if err != nil {
		return wireFrame{}, nil, err
	}
	if len(body) <= helperMaxWireEnvelope {
		return frame, body, nil
	}
	if event.Kind != "result" {
		return wireFrame{}, nil, fmt.Errorf("encoded %s frame is %d bytes, limit is %d", event.Kind, len(body), helperMaxWireEnvelope)
	}

	event = structuredHostError(
		"helper_result_too_large",
		"helper result does not fit in a wire envelope",
		helperLimitDetails("envelope", helperMaxWireEnvelope),
	)
	frame, err = helperEventFrame(id, file, event)
	if err != nil {
		return wireFrame{}, nil, err
	}
	body, err = encodeFrame(frame)
	if err != nil {
		return wireFrame{}, nil, err
	}
	if len(body) > helperMaxWireEnvelope {
		return wireFrame{}, nil, fmt.Errorf("encoded error frame is %d bytes, limit is %d", len(body), helperMaxWireEnvelope)
	}
	return frame, body, nil
}

func helperEventFrame(id, file string, event HelperEvent) (wireFrame, error) {
	frame := wireFrame{ID: id, File: file, Text: event.Text}
	switch event.Kind {
	case "status":
		frame.Type = "wire/status"
		if event.Progress != nil {
			payload, err := encodeJSON(struct {
				Progress json.RawMessage `json:"progress"`
			}{Progress: event.Progress})
			if err != nil {
				return wireFrame{}, err
			}
			frame.Payload = json.RawMessage(payload)
		}
	case "result":
		if len(event.Value) == 0 {
			return wireFrame{}, errors.New("structured result has no value")
		}
		frame.Type = "wire/done"
		frame.Payload = event.Value
	case "error":
		frame.Type = "wire/error"
		payload, err := encodeJSON(struct {
			Source  string          `json:"source"`
			Code    string          `json:"code"`
			Details json.RawMessage `json:"details,omitempty"`
		}{Source: event.Source, Code: event.Code, Details: event.Details})
		if err != nil {
			return wireFrame{}, err
		}
		frame.Payload = json.RawMessage(payload)
	default:
		return wireFrame{}, fmt.Errorf("unknown helper event kind %q", event.Kind)
	}
	return frame, nil
}

type structuredEventQueue struct {
	mu          sync.Mutex
	wake        chan struct{}
	status      *HelperEvent
	terminal    *queuedStructuredTerminal
	terminalSet bool
	send        func(HelperEvent)
}

type queuedStructuredTerminal struct {
	event HelperEvent
	sent  chan struct{}
}

func newStructuredEventQueue(send func(HelperEvent)) *structuredEventQueue {
	q := &structuredEventQueue{wake: make(chan struct{}, 1), send: send}
	go q.run()
	return q
}

func (q *structuredEventQueue) emit(event HelperEvent) {
	q.mu.Lock()
	if event.Kind == "status" {
		if !q.terminalSet {
			copy := event
			q.status = &copy
		}
		q.mu.Unlock()
		q.signal()
		return
	}

	queued := &queuedStructuredTerminal{event: event, sent: make(chan struct{})}
	if !q.terminalSet {
		q.terminalSet = true
		q.terminal = queued
	} else {
		close(queued.sent)
	}
	q.mu.Unlock()
	q.signal()
	<-queued.sent
}

func (q *structuredEventQueue) signal() {
	select {
	case q.wake <- struct{}{}:
	default:
	}
}

func (q *structuredEventQueue) run() {
	for range q.wake {
		for {
			q.mu.Lock()
			var event HelperEvent
			var sent chan struct{}
			if q.status != nil {
				event = *q.status
				q.status = nil
			} else if q.terminal != nil {
				event = q.terminal.event
				sent = q.terminal.sent
				q.terminal = nil
			} else {
				q.mu.Unlock()
				break
			}
			q.mu.Unlock()
			q.send(event)
			if sent != nil {
				close(sent)
				return
			}
		}
	}
}

package helper

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"strconv"
	"unicode/utf8"
)

const (
	ProtocolVersion     = 1
	MaxRecord           = 512 << 10
	MaxTerminalRecord   = 512 << 10
	MaxStatusRecord     = 32 << 10
	MaxWireEnvelope     = 1 << 20
	StructuredDeadline  = 5 * 60 * 1000
	DescribeDeadline    = 5 * 1000
	DescribeResultLimit = 8 << 10
	maxStatusText       = 4 << 10
	maxErrorCode        = 128
	maxProgressUnit     = 64
)

var errRecordTooLarge = errors.New("protocol record too large")

type record struct {
	typeName string
	text     string
	progress json.RawMessage
	value    json.RawMessage
	code     string
	message  string
	details  json.RawMessage
}

func readRecord(r *bufio.Reader, max int) (record, error) {
	raw, err := readRecordLine(r, max)
	if err != nil {
		return record{}, err
	}
	if !utf8.Valid(raw) {
		return record{}, errors.New("record is not valid UTF-8")
	}
	if len(raw) == 0 {
		return record{}, errors.New("blank protocol record")
	}

	fields, err := recordFields(raw)
	if err != nil {
		return record{}, err
	}
	typeName, err := requiredRecordString(fields, "type")
	if err != nil {
		return record{}, err
	}
	record := record{typeName: typeName}

	switch typeName {
	case "status":
		if len(raw) > MaxStatusRecord {
			return record, fmt.Errorf("status record is %d bytes, limit is %d", len(raw), MaxStatusRecord)
		}
		record.text, err = requiredRecordString(fields, "text")
		if err != nil {
			return record, err
		}
		if len(record.text) > maxStatusText {
			return record, fmt.Errorf("status.text is %d bytes, limit is %d", len(record.text), maxStatusText)
		}
		if progress, ok := fields["progress"]; ok {
			if err := validateProgress(progress); err != nil {
				return record, err
			}
			record.progress = progress
		}
	case "result":
		value, ok := fields["value"]
		if !ok {
			return record, errors.New("result.value is required")
		}
		if len(raw) > MaxTerminalRecord {
			return record, fmt.Errorf("result record is %d bytes, limit is %d", len(raw), MaxTerminalRecord)
		}
		record.value = value
	case "error":
		if len(raw) > MaxTerminalRecord {
			return record, fmt.Errorf("error record is %d bytes, limit is %d", len(raw), MaxTerminalRecord)
		}
		record.code, err = requiredRecordString(fields, "code")
		if err != nil {
			return record, err
		}
		if record.code == "" {
			return record, errors.New("error.code must not be empty")
		}
		if len(record.code) > maxErrorCode {
			return record, fmt.Errorf("error.code is %d bytes, limit is %d", len(record.code), maxErrorCode)
		}
		record.message, err = requiredRecordString(fields, "message")
		if err != nil {
			return record, err
		}
		if record.message == "" {
			return record, errors.New("error.message must not be empty")
		}
		if len(record.message) > maxStatusText {
			return record, fmt.Errorf("error.message is %d bytes, limit is %d", len(record.message), maxStatusText)
		}
		record.details = fields["details"]
	default:
		return record, fmt.Errorf("unknown record type %q", typeName)
	}

	return record, nil
}

func readRecordLine(r *bufio.Reader, max int) ([]byte, error) {
	var raw []byte
	for {
		chunk, err := r.ReadSlice('\n')
		if errors.Is(err, bufio.ErrBufferFull) {
			if len(raw)+len(chunk) > max {
				return nil, errRecordTooLarge
			}
			raw = append(raw, chunk...)
			continue
		}

		hasNewline := len(chunk) > 0 && chunk[len(chunk)-1] == '\n'
		lineBytes := len(raw) + len(chunk)
		if hasNewline {
			lineBytes--
			if lineBytes > 0 {
				last := byte(0)
				if len(chunk) > 1 {
					last = chunk[len(chunk)-2]
				} else {
					last = raw[len(raw)-1]
				}
				if last == '\r' {
					lineBytes--
				}
			}
		}
		if lineBytes > max {
			return nil, errRecordTooLarge
		}
		raw = append(raw, chunk...)
		if hasNewline {
			raw = raw[:len(raw)-1]
			if len(raw) > 0 && raw[len(raw)-1] == '\r' {
				raw = raw[:len(raw)-1]
			}
		}
		if err != nil && !errors.Is(err, io.EOF) {
			return nil, err
		}
		if errors.Is(err, io.EOF) && len(raw) == 0 {
			return nil, io.EOF
		}
		return raw, nil
	}
}

func recordFields(raw []byte) (map[string]json.RawMessage, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	first, err := dec.Token()
	if err != nil {
		return nil, fmt.Errorf("invalid JSON record: %w", err)
	}
	if delim, ok := first.(json.Delim); !ok || delim != '{' {
		return nil, errors.New("protocol record must be a JSON object")
	}

	fields := make(map[string]json.RawMessage)
	for dec.More() {
		token, err := dec.Token()
		if err != nil {
			return nil, fmt.Errorf("invalid JSON record: %w", err)
		}
		key, ok := token.(string)
		if !ok {
			return nil, errors.New("protocol record contains a non-string key")
		}
		if _, exists := fields[key]; exists {
			return nil, fmt.Errorf("duplicate top-level key %q", key)
		}
		var value json.RawMessage
		if err := dec.Decode(&value); err != nil {
			return nil, fmt.Errorf("invalid value for %q: %w", key, err)
		}
		fields[key] = value
	}
	if _, err := dec.Token(); err != nil {
		return nil, fmt.Errorf("invalid JSON record: %w", err)
	}
	var extra json.RawMessage
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return nil, errors.New("protocol line contains more than one JSON value")
		}
		return nil, fmt.Errorf("invalid JSON record: %w", err)
	}
	return fields, nil
}

func requiredRecordString(fields map[string]json.RawMessage, name string) (string, error) {
	raw, ok := fields[name]
	if !ok {
		return "", fmt.Errorf("%s is required", name)
	}
	// Decode through any and assert, for the same reason as progress.unit:
	// json.Unmarshal accepts null into a string target and yields "", so a
	// null field would read as a present empty string.
	var decoded any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return "", fmt.Errorf("%s must be a string", name)
	}
	value, ok := decoded.(string)
	if !ok {
		return "", fmt.Errorf("%s must be a string", name)
	}
	return value, nil
}

func validateProgress(raw json.RawMessage) error {
	fields, err := recordFields(raw)
	if err != nil {
		return fmt.Errorf("status.progress must be an object: %w", err)
	}
	completed, ok := fields["completed"]
	if !ok {
		return errors.New("status.progress.completed is required")
	}
	if err := validateNonnegativeNumber(completed, "status.progress.completed"); err != nil {
		return err
	}
	if total, ok := fields["total"]; ok && !bytes.Equal(bytes.TrimSpace(total), []byte("null")) {
		if err := validateNonnegativeNumber(total, "status.progress.total"); err != nil {
			return err
		}
	}
	if unit, ok := fields["unit"]; ok {
		// Decode through any and assert. json.Unmarshal treats null as a no-op
		// for a string target, so unmarshalling straight into one accepts
		// "unit":null and leaves the value empty, which is the same
		// null-is-not-absent trap the result.value rule exists for.
		var decoded any
		if err := json.Unmarshal(unit, &decoded); err != nil {
			return errors.New("status.progress.unit must be a string")
		}
		value, ok := decoded.(string)
		if !ok {
			return errors.New("status.progress.unit must be a string")
		}
		if len(value) > maxProgressUnit {
			return fmt.Errorf("status.progress.unit is %d bytes, limit is %d", len(value), maxProgressUnit)
		}
	}
	return nil
}

func validateNonnegativeNumber(raw json.RawMessage, name string) error {
	// Decode through any and assert, for the same reason as progress.unit.
	// json.Unmarshal into a json.Number accepts a JSON STRING holding digits, so
	// "completed":"4" satisfied a grammar that requires a number and the progress
	// object is forwarded to the client unchanged: a status callback that adds
	// completed to total would concatenate instead.
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var decoded any
	if err := dec.Decode(&decoded); err != nil {
		return fmt.Errorf("%s must be a number", name)
	}
	number, ok := decoded.(json.Number)
	if !ok {
		return fmt.Errorf("%s must be a number", name)
	}
	value, err := strconv.ParseFloat(number.String(), 64)
	if err != nil || math.IsInf(value, 0) || math.IsNaN(value) || value < 0 {
		return fmt.Errorf("%s must be a nonnegative finite number", name)
	}
	return nil
}

func jsonBytes(value any) []byte {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(value); err != nil {
		return nil
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n"))
}

func rawJSON(value any) json.RawMessage {
	return json.RawMessage(jsonBytes(value))
}

// StampProtocol produces exactly what the child reads on standard input: the
// accepted wire envelope, the helper protocol version, and the document mode the
// host resolved. The mode is written even when the page omitted the field, so
// the program branches on the same value the host is enforcing.
func StampProtocol(raw []byte, document string) ([]byte, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, err
	}
	if fields == nil {
		return nil, errors.New("request envelope must be an object")
	}
	fields["helperProtocol"] = json.RawMessage(strconv.Itoa(ProtocolVersion))
	fields["document"] = rawJSON(document)
	return jsonBytes(fields), nil
}

func failureDetails(field, reason string) json.RawMessage {
	return rawJSON(struct {
		Field  string `json:"field"`
		Reason string `json:"reason"`
	}{Field: field, Reason: reason})
}

func limitDetails(field string, limit int) json.RawMessage {
	return rawJSON(struct {
		Field string `json:"field"`
		Limit int    `json:"limit"`
	}{Field: field, Limit: limit})
}

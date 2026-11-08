package gates

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/hivecommons/hive/crux/logscrub"
)

// maxIdempotencyKeyLen bounds a caller-supplied idempotency key.
const maxIdempotencyKeyLen = 256

// normalize validates req and returns the form that is stored and keyed:
// trimmed text, default choices filled in, the context re-encoded
// canonically (sorted object keys) and every free-text field scrubbed of
// credential-shaped strings.
func normalize(req Request) (Request, error) {
	out := Request{Timeout: req.Timeout, IdempotencyKey: strings.TrimSpace(req.IdempotencyKey)}
	subject := strings.TrimSpace(req.Subject)
	switch {
	case subject == "":
		return Request{}, fmt.Errorf("%w: subject is required", ErrBadRequest)
	case len(subject) > MaxSubjectLen:
		return Request{}, fmt.Errorf("%w: subject is %d bytes, over %d", ErrBadRequest, len(subject), MaxSubjectLen)
	}
	out.Subject = logscrub.ScrubString(subject)
	openedBy := strings.TrimSpace(req.OpenedBy)
	if len(openedBy) > MaxOpenedByLen {
		return Request{}, fmt.Errorf("%w: opened_by is %d bytes, over %d", ErrBadRequest, len(openedBy), MaxOpenedByLen)
	}
	out.OpenedBy = logscrub.ScrubString(openedBy)
	if req.Timeout < 0 {
		return Request{}, fmt.Errorf("%w: timeout is negative", ErrBadRequest)
	}
	if len(out.IdempotencyKey) > maxIdempotencyKeyLen {
		return Request{}, fmt.Errorf("%w: idempotency key is over %d bytes", ErrBadRequest, maxIdempotencyKeyLen)
	}
	if out.Run = strings.TrimSpace(req.Run); len(out.Run) > MaxRunLen {
		return Request{}, fmt.Errorf("%w: run is %d bytes, over %d", ErrBadRequest, len(out.Run), MaxRunLen)
	}
	choices, err := normalizeChoices(req.Choices)
	if err != nil {
		return Request{}, err
	}
	out.Choices = choices
	if out.Context, err = normalizeContext(req.Context); err != nil {
		return Request{}, err
	}
	return out, nil
}

func normalizeChoices(in []string) ([]string, error) {
	if len(in) == 0 {
		return append([]string(nil), DefaultChoices...), nil
	}
	if len(in) > MaxChoices {
		return nil, fmt.Errorf("%w: %d choices, over %d", ErrBadRequest, len(in), MaxChoices)
	}
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, c := range in {
		c = strings.TrimSpace(c)
		switch {
		case c == "":
			return nil, fmt.Errorf("%w: empty choice", ErrBadRequest)
		case len(c) > MaxChoiceLen:
			return nil, fmt.Errorf("%w: choice %q is over %d bytes", ErrBadRequest, c[:MaxChoiceLen], MaxChoiceLen)
		case seen[c]:
			return nil, fmt.Errorf("%w: duplicate choice %q", ErrBadRequest, c)
		}
		seen[c] = true
		out = append(out, c)
	}
	return out, nil
}

// normalizeContext checks the payload is one JSON value no larger than
// MaxContextBytes once encoded, then returns it canonically encoded with
// its string values scrubbed. JSON null and an absent payload are nil.
func normalizeContext(raw json.RawMessage) (json.RawMessage, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, fmt.Errorf("%w: context is not JSON: %v", ErrBadRequest, err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("%w: context must be a single JSON value", ErrBadRequest)
	}
	if v == nil {
		return nil, nil
	}
	encoded, err := encodeCanonical(v)
	if err != nil {
		return nil, fmt.Errorf("%w: context: %v", ErrBadRequest, err)
	}
	if len(encoded) > MaxContextBytes {
		return nil, fmt.Errorf("%w: %d bytes encoded, over %d", ErrContextTooLarge, len(encoded), MaxContextBytes)
	}
	return encodeCanonical(scrubValue(v))
}

// encodeCanonical marshals v with sorted object keys and without HTML
// escaping, so the stored payload is the driver's text.
func encodeCanonical(v any) (json.RawMessage, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return json.RawMessage(bytes.TrimRight(buf.Bytes(), "\n")), nil
}

// scrubValue redacts credential-shaped strings in every string leaf.
func scrubValue(v any) any {
	switch t := v.(type) {
	case string:
		return logscrub.ScrubString(t)
	case map[string]any:
		for k, e := range t {
			t[k] = scrubValue(e)
		}
		return t
	case []any:
		for i, e := range t {
			t[i] = scrubValue(e)
		}
		return t
	}
	return v
}

// normalizeNote trims and bounds a note and scrubs it.
func normalizeNote(note string) (string, error) {
	note = strings.TrimSpace(note)
	if len(note) > MaxNoteLen {
		return "", fmt.Errorf("%w: note is %d bytes, over %d", ErrBadRequest, len(note), MaxNoteLen)
	}
	return logscrub.ScrubString(note), nil
}

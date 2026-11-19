package prosefilter

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
)

// Kind says how a JSON field's text is filtered.
type Kind int

const (
	// Prose is multi-line Markdown text: Sanitize.
	Prose Kind = iota + 1
	// Title is single-line text: SanitizeTitle.
	Title
)

// Keys maps a JSON object key to the filter its string values get.
type Keys map[string]Kind

// Stats counts what SanitizeJSON changed: text fields rewritten, and dashes
// removed. It never carries the text itself, so it is safe to log.
type Stats struct {
	Fields int
	Dashes int
}

// AgentWritesEnv is the operator switch for rewriting text that an agent
// wrote itself (its GitHub writes through the egress proxy, its HEAD commit
// message in the push broker). It is on unless set to one of
// agentWritesOffValues. Text hive itself composes is always filtered.
const AgentWritesEnv = "HIVE_AGENT_PROSE_FILTER"

var agentWritesOffValues = map[string]bool{"false": true, "0": true, "off": true, "no": true}

// AgentWritesEnabled reports whether AgentWritesEnv leaves the agent-write
// rewrite on.
func AgentWritesEnabled() bool {
	return !agentWritesOffValues[strings.ToLower(strings.TrimSpace(os.Getenv(AgentWritesEnv)))]
}

// SanitizeJSON rewrites, at any depth of a JSON document, the string values
// (and the strings of a string array) stored under a key in keys, using the
// same rule as Sanitize. Every other value, number formatting included, is
// preserved. A body that is not JSON, holds no dash, or has nothing to change
// is returned unchanged with zero Stats; only a changed body is re-encoded
// (keys sorted, no HTML escaping).
func SanitizeJSON(body []byte, keys Keys) ([]byte, Stats) {
	if len(keys) == 0 || !mayHoldDash(body) {
		return body, Stats{}
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var doc any
	if dec.Decode(&doc) != nil || dec.More() {
		return body, Stats{}
	}
	var st Stats
	doc = sanitizeTree(doc, keys, &st)
	if st.Fields == 0 {
		return body, Stats{}
	}
	out, err := marshalNoEscape(doc)
	if err != nil {
		return body, Stats{}
	}
	return out, st
}

// mayHoldDash reports whether body could contain an em- or en-dash, raw or as
// a JSON \u escape.
func mayHoldDash(body []byte) bool {
	if bytes.Contains(body, []byte(emDash)) || bytes.Contains(body, []byte(enDash)) {
		return true
	}
	lower := bytes.ToLower(body)
	return bytes.Contains(lower, []byte(`—`)) || bytes.Contains(lower, []byte(`–`))
}

func sanitizeTree(v any, keys Keys, st *Stats) any {
	switch t := v.(type) {
	case map[string]any:
		for k, child := range t {
			if kind, ok := keys[k]; ok {
				switch c := child.(type) {
				case string:
					t[k] = sanitizeCounted(c, kind, st)
					continue
				case []any:
					if allStrings(c) {
						for i, s := range c {
							c[i] = sanitizeCounted(s.(string), kind, st)
						}
						continue
					}
				}
			}
			t[k] = sanitizeTree(child, keys, st)
		}
		return t
	case []any:
		for i, child := range t {
			t[i] = sanitizeTree(child, keys, st)
		}
		return t
	default:
		return v
	}
}

func allStrings(items []any) bool {
	for _, it := range items {
		if _, ok := it.(string); !ok {
			return false
		}
	}
	return len(items) > 0
}

func sanitizeCounted(s string, kind Kind, st *Stats) string {
	if !HasDash(s) {
		return s
	}
	var clean string
	if kind == Title {
		clean = SanitizeTitle(s)
	} else {
		clean = Sanitize(s)
	}
	if clean != s {
		st.Fields++
		st.Dashes += DashCount(s) - DashCount(clean)
	}
	return clean
}

// DashCount is the number of em-dashes and en-dashes in s.
func DashCount(s string) int {
	return strings.Count(s, emDash) + strings.Count(s, enDash)
}

// marshalNoEscape is json.Marshal without HTML escaping, so a body that only
// had its dashes rewritten does not also gain < escapes.
func marshalNoEscape(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

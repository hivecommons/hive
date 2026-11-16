package betty

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/meetings/meetllm"
)

// Answer limits and timeouts.
const (
	// DefaultUnpromptedTimeout bounds the model call for a room question.
	DefaultUnpromptedTimeout = 2500 * time.Millisecond
	// DefaultAddressedTimeout bounds the model call when addressed.
	DefaultAddressedTimeout = 4 * time.Second
	// DefaultRestyleTimeout bounds restyling one of Spek's questions.
	DefaultRestyleTimeout = 800 * time.Millisecond
	// ChatMaxSentences and ChatMaxChars bound a chat answer.
	ChatMaxSentences = 3
	ChatMaxChars     = 300
	// restyleMaxChars bounds a restyled question.
	restyleMaxChars = 200
)

// LLM is one model call with a system prompt.
type LLM interface {
	Complete(ctx context.Context, system, prompt string) (string, error)
}

// MeetLLM calls the meetings model through meetllm, charging the meeting's
// usage file and honouring its token budget.
type MeetLLM struct {
	Config meetllm.Config
	Client *http.Client
}

// Complete implements LLM.
func (m MeetLLM) Complete(ctx context.Context, system, prompt string) (string, error) {
	c := m.Config
	c.System = system
	if c.Timeout <= 0 {
		c.Timeout = DefaultAddressedTimeout
	}
	if c.MaxInFlight <= 0 {
		c.MaxInFlight = 1
	}
	hc := m.Client
	if hc == nil {
		hc = http.DefaultClient
	}
	return meetllm.Complete(ctx, c, hc, prompt)
}

// Answer is the model's structured reply.
type Answer struct {
	Answer        string   `json:"answer"`
	Confidence    float64  `json:"confidence"`
	NeedsResearch bool     `json:"needs_research"`
	Sources       []string `json:"sources"`
}

// errNoJSON is returned when the reply holds no JSON object.
var errNoJSON = errors.New("betty: model reply is not a JSON answer")

// ParseAnswer reads the model's JSON answer, tolerating code fences and
// text around the object. Sources may be numbers or strings.
func ParseAnswer(raw string) (Answer, error) {
	i, j := strings.Index(raw, "{"), strings.LastIndex(raw, "}")
	if i < 0 || j <= i {
		return Answer{}, errNoJSON
	}
	var loose struct {
		Answer        string            `json:"answer"`
		Confidence    float64           `json:"confidence"`
		NeedsResearch bool              `json:"needs_research"`
		Sources       []json.RawMessage `json:"sources"`
	}
	if err := json.Unmarshal([]byte(raw[i:j+1]), &loose); err != nil {
		return Answer{}, errNoJSON
	}
	a := Answer{Answer: strings.TrimSpace(loose.Answer), Confidence: loose.Confidence, NeedsResearch: loose.NeedsResearch}
	for _, s := range loose.Sources {
		var str string
		if json.Unmarshal(s, &str) == nil {
			a.Sources = append(a.Sources, str)
		} else {
			a.Sources = append(a.Sources, string(s))
		}
	}
	a.Confidence = max(0, min(1, a.Confidence))
	return a, nil
}

// ClipChat bounds a chat answer to three sentences and 300 characters.
func ClipChat(text string) string {
	text = strings.Join(strings.Fields(text), " ")
	n, cut := 0, -1
	for i, r := range text {
		if r == '.' || r == '!' || r == '?' {
			if i+1 == len(text) || text[i+1] == ' ' {
				n++
				if n == ChatMaxSentences {
					cut = i + 1
					break
				}
			}
		}
	}
	if cut > 0 {
		text = text[:cut]
	}
	return clip(text, ChatMaxChars)
}

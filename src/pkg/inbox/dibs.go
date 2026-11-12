package inbox

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// dibsTimeout bounds one idea submission.
const dibsTimeout = 30 * time.Second

// dibsMaxBody caps the idea body so a long transcript does not trip dibs'
// request limit; the full text stays in Knowledge.
const dibsMaxBody = 60_000

// DibsDestination posts an item as a private draft idea to a dibs instance
// (`POST {url}/api/ideas`, bearer hub token).
type DibsDestination struct {
	URL   string
	Token string
	HTTP  *http.Client
}

// Name implements Destination.
func (d *DibsDestination) Name() string { return DestDibs }

// ErrDibsUnconfigured is returned when inbox.dibs.url is empty.
var ErrDibsUnconfigured = errors.New("dibs: inbox.dibs.url is not configured")

type dibsIdea struct {
	Title      string `json:"title"`
	Body       string `json:"body"`
	Visibility string `json:"visibility"`
	Status     string `json:"status"`
}

// Deliver implements Destination.
func (d *DibsDestination) Deliver(ctx context.Context, it Item, c Content, _ Request) (Result, error) {
	base := strings.TrimRight(strings.TrimSpace(d.URL), "/")
	if base == "" {
		return Result{}, ErrDibsUnconfigured
	}
	if strings.TrimSpace(d.Token) == "" {
		return Result{}, errors.New("dibs: no token (set inbox.dibs.token_env to a variable holding a hub bearer token)")
	}
	body := DibsBody(it, c)
	payload, _ := json.Marshal(dibsIdea{
		Title:      firstNonEmpty(c.Title, it.Title),
		Body:       body,
		Visibility: "private",
		Status:     "draft",
	})
	rctx, cancel := context.WithTimeout(ctx, dibsTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(rctx, http.MethodPost, base+"/api/ideas", bytes.NewReader(payload))
	if err != nil {
		return Result{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+d.Token)
	client := d.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return Result{}, fmt.Errorf("dibs: %w", err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return Result{}, fmt.Errorf("dibs: %s: %s", resp.Status, lastLines(string(data), 2))
	}
	var idea struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(data, &idea); err != nil || idea.ID == "" {
		return Result{}, fmt.Errorf("dibs: response has no idea id: %s", lastLines(string(data), 2))
	}
	return Result{URL: base + "/api/ideas/" + idea.ID}, nil
}

// DibsBody renders the idea body: summary, provenance, then transcript.
func DibsBody(it Item, c Content) string {
	var b strings.Builder
	if c.Summary != "" {
		b.WriteString(c.Summary)
		b.WriteString("\n\n")
	}
	fmt.Fprintf(&b, "_Source: %s_\n", c.Provenance)
	if !it.RecordedAt.IsZero() {
		fmt.Fprintf(&b, "_Recorded: %s_\n", it.RecordedAt.UTC().Format(time.RFC3339))
	}
	text := c.Transcript
	if text == "" && c.Summary == "" {
		text = c.Markdown
	}
	if text != "" {
		b.WriteString("\n## Transcript\n\n")
		b.WriteString(text)
	}
	out := b.String()
	if len(out) > dibsMaxBody {
		out = out[:dibsMaxBody] + "\n\n_(truncated; full transcript in Knowledge)_"
	}
	return out
}

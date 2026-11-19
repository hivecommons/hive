package betty

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"time"
)

// Direct sources: keyless public APIs for common "live fact" questions,
// asked before (and beside) a general web search because they answer in one
// or two short calls and name their source: Wikipedia for definitions and
// entities, Open-Meteo for weather, and the GitHub releases API for "latest
// release of <project>". They are public-web calls: no credential of the
// hive is ever attached, and a question about the hive never reaches them
// (WebGuard).

// Direct source defaults.
const (
	// DefaultDirectTimeout bounds one direct answer (every call it makes),
	// inside the retrieval deadline (DefaultRetrieveTimeout).
	DefaultDirectTimeout = 1400 * time.Millisecond
	// directCallTimeout bounds one HTTP call of a direct answer.
	directCallTimeout = 900 * time.Millisecond
	// maxDirectResponseBytes bounds one direct response read.
	maxDirectResponseBytes = 1 << 20
	// DefaultDirectUserAgent names the client generically: no person,
	// project or hive.
	DefaultDirectUserAgent = "meeting-assistant/1.0 (automated; keyless public lookup)"
	// maxDirectTextRunes bounds a direct snippet's text.
	maxDirectTextRunes = 600
	// directName is DirectRetriever's name.
	directName = "direct"
)

// Direct source endpoints.
const (
	defaultWikipediaURL = "https://en.wikipedia.org"
	defaultGeocodeURL   = "https://geocoding-api.open-meteo.com"
	defaultForecastURL  = "https://api.open-meteo.com"
	defaultGitHubAPIURL = "https://api.github.com"
)

// Direct source names, as logged and cited.
const (
	SourceWikipedia      = "Wikipedia"
	SourceOpenMeteo      = "Open-Meteo"
	SourceNWS            = "National Weather Service"
	SourceGitHubReleases = "GitHub releases"
)

// DirectRetriever answers weather, latest-release and definition questions
// from keyless public APIs. Its results are untrusted data, like any web
// result.
type DirectRetriever struct {
	// Client makes the calls. It must be a plain client: never one that
	// adds the hive's GitHub token or any other credential.
	Client *http.Client
	// Guard keeps hive questions off the public web and cleans the query.
	Guard  *WebGuard
	Logger *slog.Logger
	// Timeout bounds one answer; zero means DefaultDirectTimeout.
	Timeout time.Duration
	// UserAgent overrides DefaultDirectUserAgent.
	UserAgent string
	// WikipediaURL, GeocodeURL, ForecastURL and GitHubAPIURL override the
	// public endpoints (tests).
	WikipediaURL, GeocodeURL, ForecastURL, GitHubAPIURL string
	// NWSURL overrides the National Weather Service API root (tests).
	NWSURL string
	// Now is the clock for observation freshness; nil means time.Now.
	Now func() time.Time
}

// Name implements Retriever.
func (DirectRetriever) Name() string { return directName }

// Public implements PublicRetriever.
func (DirectRetriever) Public() bool { return true }

// Retrieve implements Retriever: at most one source answers a question.
func (d DirectRetriever) Retrieve(ctx context.Context, query string, _ []string, limit int) []Snippet {
	if limit <= 0 {
		return nil
	}
	c, blocked := d.Guard.check(query)
	if blocked != "" {
		logWebBlocked(d.Logger, directName, blocked)
		return nil
	}
	return d.answer(ctx, c)
}

// answer asks the one source that fits a cleaned, allowed query, if any.
func (d DirectRetriever) answer(ctx context.Context, c cleanedQuery) []Snippet {
	timeout := d.Timeout
	if timeout <= 0 {
		timeout = DefaultDirectTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	start := time.Now()
	var (
		source string
		out    []Snippet
	)
	if place, ok := weatherPlace(c.Query); ok {
		if place == "" {
			// She cannot know where the speaker is: no call is made.
			return []Snippet{needsPlaceSnippet()}
		}
		source, out = SourceOpenMeteo, d.weather(ctx, place)
		if len(out) > 0 {
			source = out[0].Source
		}
	} else if repo, ok := releaseRepo(c.Query); ok {
		source, out = SourceGitHubReleases, d.release(ctx, repo)
	} else if topic, ok := definitionTopic(c); ok {
		source, out = SourceWikipedia, d.wikipedia(ctx, topic)
	} else {
		return nil
	}
	logWebRetrieval(d.Logger, source, c.Query, len(out), time.Since(start))
	return out
}

// base is an endpoint override, or its default.
func base(override, def string) string {
	if override != "" {
		return override
	}
	return def
}

// getJSON fetches one JSON document into v with the generic User-Agent, no
// credentials, a bounded read and the per-call timeout.
func (d DirectRetriever) getJSON(ctx context.Context, u string, v any) bool {
	ctx, cancel := context.WithTimeout(ctx, directCallTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return false
	}
	ua := d.UserAgent
	if ua == "" {
		ua = DefaultDirectUserAgent
	}
	req.Header.Set("User-Agent", ua)
	req.Header.Set("Accept", "application/json")
	hc := d.Client
	if hc == nil {
		hc = http.DefaultClient
	}
	resp, err := hc.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false
	}
	return json.NewDecoder(io.LimitReader(resp.Body, maxDirectResponseBytes)).Decode(v) == nil
}

// logWebRetrieval is the one line per web retrieval: the source or engine,
// the result count and the elapsed time. The cleaned query is logged, never
// the utterance.
func logWebRetrieval(log *slog.Logger, source, query string, results int, elapsed time.Duration, more ...any) {
	if log == nil {
		return
	}
	args := append([]any{"source", source, "results", results, "elapsed_ms", elapsed.Milliseconds(), "query", query}, more...)
	log.Info("[meetings] persona web retrieval", args...)
}

// logWebBlocked records, without the question, that one was kept off the
// public web.
func logWebBlocked(log *slog.Logger, source, reason string) {
	if log == nil || reason == webBlockedEmpty {
		return
	}
	log.Debug("[meetings] persona question kept off the public web", "source", source, "reason", reason)
}

// PublicWeb configures a persona's public-web retrievers.
type PublicWeb struct {
	// SearxngURL is the SearXNG root; empty turns every public-web
	// retriever off (web answers are opt-in).
	SearxngURL string
	Client     *http.Client
	Guard      *WebGuard
	Logger     *slog.Logger
}

// PublicWebRetrievers are the retrievers that reach the public web: one
// WebRetriever that asks the direct sources and SearXNG together. None when
// web answers are off.
func PublicWebRetrievers(p PublicWeb) []Retriever {
	if p.SearxngURL == "" {
		return nil
	}
	direct := DirectRetriever{Client: p.Client, Logger: p.Logger}
	return []Retriever{WebRetriever{BaseURL: p.SearxngURL, Client: p.Client, Guard: p.Guard, Logger: p.Logger, Direct: &direct}}
}

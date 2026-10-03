package worksource

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync/atomic"
	"time"

	"github.com/hivecommons/hive/pkg/config"
)

// ExternalContract is the wire-contract identifier Hive sends and requires in
// every response. A provider that answers with anything else is talking a
// contract this binary does not implement, which is an error, not a hint to
// guess (ADR-0020).
const ExternalContract = "hive.worksource/v1"

const (
	// ExternalContractHeader carries ExternalContract on every request so a
	// provider can route or reject by version without parsing the path.
	ExternalContractHeader = "Hive-Worksource-Contract"
	// externalSourcePath is the one-shot handshake endpoint. It is called once
	// per process, never per cycle.
	externalSourcePath = "/v1/source"
	// externalIssuesPath is the paginated enumeration endpoint.
	externalIssuesPath = "/v1/issues"
)

// Limits on one ListIssues call. They are constants, not config: raising them
// changes what a provider may make the hub hold in memory, which is a contract
// revision rather than an operator setting.
const (
	externalMaxPages     = 50
	externalMaxItems     = 5000
	externalMaxBodyBytes = 8 << 20
	// externalErrorBodyLimit bounds how much of a provider error body is
	// carried into a Hive log line.
	externalErrorBodyLimit = 512
	// externalMaxRedirects bounds same-host redirect chains.
	externalMaxRedirects = 10
)

// externalIDPattern is the allowed shape of a provider's native identifier.
//
// It forbids "!", "#", ":", "/" and whitespace, which is what makes the
// resulting Ref.Key() unambiguous: it can only ever parse back as the
// "repo!externalID" form, never as a GitHub "repo#N" key and never as a
// run-stage key ("<run>:<stage>"). An external provider therefore cannot take
// over the identity of GitHub-backed work, a run stage, or a Wavefront node.
var externalIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

// externalPriorities is the normalized priority enum. Anything else becomes "":
// the wire has no way to raise an item above the levels Hive already knows.
var externalPriorities = map[string]struct{}{
	"urgent": {}, "high": {}, "medium": {}, "low": {}, "none": {},
}

// ExternalConfig is the resolved form of config.ExternalSourceConfig: secrets
// are real values here, resolved by FromConfig and held only in memory.
type ExternalConfig struct {
	Name        string
	DisplayName string
	BaseURL     string
	AuthToken   string
	CABundle    string
	Repos       []string
	HoldLabels  []string
	Timeout     time.Duration
	Logger      *slog.Logger
}

// externalSource is the read-only HTTP/JSON primary work source. It implements
// WorkSource and deliberately implements none of LabelMutator, Commenter, or
// StatusTransitioner: hive.worksource/v1 has no write-back, and design mode
// already renders a missing interface as "unsupported".
type externalSource struct {
	cfg    ExternalConfig
	client *http.Client

	repos map[string]struct{}
	holds map[string]struct{}

	handshakeOK atomic.Bool

	dropped atomic.Int64
}

// NewExternalSource builds the external adapter. It does no network I/O: the
// provider handshake happens on the first ListIssues (and on an explicit
// CheckConnection), so a provider outage never blocks hub startup — it just
// makes that cycle fail closed.
func NewExternalSource(cfg ExternalConfig) (WorkSource, error) {
	if strings.TrimSpace(cfg.Name) == "" {
		return nil, fmt.Errorf("work_source.external.name is required")
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = time.Duration(config.DefaultExternalTimeoutSeconds) * time.Second
	}
	client, err := externalHTTPClient(cfg)
	if err != nil {
		return nil, err
	}
	src := &externalSource{
		cfg:    cfg,
		client: client,
		repos:  make(map[string]struct{}, len(cfg.Repos)),
		holds:  make(map[string]struct{}, len(cfg.HoldLabels)),
	}
	for _, repo := range cfg.Repos {
		if r := strings.TrimSpace(repo); r != "" {
			src.repos[r] = struct{}{}
		}
	}
	for _, label := range cfg.HoldLabels {
		if l := strings.TrimSpace(label); l != "" {
			src.holds[l] = struct{}{}
		}
	}
	return src, nil
}

// SourceType returns the configured name and makes no network call: it is read
// on every log line and dashboard badge.
func (s *externalSource) SourceType() string { return s.cfg.Name }

// DisplayName is the dashboard label, falling back to the source type.
func (s *externalSource) DisplayName() string {
	if n := strings.TrimSpace(s.cfg.DisplayName); n != "" {
		return n
	}
	return s.cfg.Name
}

// DroppedItems reports how many items this source has withheld for failing a
// validation rule, for the per-source dashboard counter.
func (s *externalSource) DroppedItems() int64 { return s.dropped.Load() }

// CheckConnection performs the /v1/source handshake on demand. It backs the
// dashboard's "test connection" action.
func (s *externalSource) CheckConnection(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, s.cfg.Timeout)
	defer cancel()
	return s.handshake(ctx)
}

// ListIssues enumerates the provider's actionable items.
//
// Every failure path returns an error rather than a partial list. The caller
// (workSourceIssuesForCycle) turns that into the fail-closed branch: no issues
// this cycle, GitHub PR maintenance untouched. A short list built from a
// half-read provider would instead look like "that work is finished" and let
// holds and claims drift.
func (s *externalSource) ListIssues(ctx context.Context) ([]Issue, error) {
	ctx, cancel := context.WithTimeout(ctx, s.cfg.Timeout)
	defer cancel()

	if err := s.handshake(ctx); err != nil {
		return nil, err
	}

	var (
		out      []Issue
		received int
		seen     = map[string]struct{}{}
		cursor   string
	)
	for page := 1; ; page++ {
		if page > externalMaxPages {
			return nil, s.errorf("provider returned more than %d pages", externalMaxPages)
		}
		var payload externalIssuesResponse
		if err := s.getJSON(ctx, s.issuesURL(cursor), &payload); err != nil {
			return nil, err
		}
		if payload.Contract != ExternalContract {
			return nil, s.errorf("%s answered with contract %q (want %q)", externalIssuesPath, payload.Contract, ExternalContract)
		}
		received += len(payload.Items)
		if received > externalMaxItems {
			return nil, s.errorf("provider returned more than %d items", externalMaxItems)
		}
		for _, item := range payload.Items {
			issue, err := s.toIssue(item)
			if err != nil {
				s.drop(item, err)
				continue
			}
			if s.heldByLabel(issue.Labels) {
				continue
			}
			key := RefFromIssue(issue).Key()
			if _, dup := seen[key]; dup {
				// Keeping either copy would be a guess about which one the
				// holds, cooldowns, and claims stored under this key belong to.
				return nil, s.errorf("two items share work key %q in one listing", key)
			}
			seen[key] = struct{}{}
			out = append(out, issue)
		}
		cursor = strings.TrimSpace(payload.NextCursor)
		if cursor == "" {
			break
		}
	}
	return out, nil
}

// handshake calls /v1/source at most once successfully per process. A failed
// attempt is not cached: a provider that was down at the first cycle must be
// able to come back without a hub restart.
func (s *externalSource) handshake(ctx context.Context) error {
	if s.handshakeOK.Load() {
		return nil
	}
	var payload externalSourceResponse
	if err := s.getJSON(ctx, s.endpoint(externalSourcePath), &payload); err != nil {
		return err
	}
	if payload.Contract != ExternalContract {
		return s.errorf("%s answered with contract %q (want %q)", externalSourcePath, payload.Contract, ExternalContract)
	}
	if payload.SourceType != s.cfg.Name {
		return s.errorf("%s reports source_type %q, but this work source is configured as %q", externalSourcePath, payload.SourceType, s.cfg.Name)
	}
	s.handshakeOK.Store(true)
	return nil
}

// toIssue validates one wire item and converts it. The returned error is the
// drop reason; it names the offending field and never carries a credential.
//
// Validation happens BEFORE a Ref is built, because the Ref key is what gets
// persisted into holds, cooldowns, claims, and queue order.
func (s *externalSource) toIssue(item externalWireItem) (Issue, error) {
	// Fields Hive owns are not accepted from the wire. A provider that sends
	// one is trying to decide identity or run staging, so the item is dropped
	// rather than sanitized.
	if strings.TrimSpace(item.SourceType) != "" {
		return Issue{}, fmt.Errorf("source_type is set by Hive and must not appear on the wire")
	}
	if item.Number != 0 {
		return Issue{}, fmt.Errorf("number is set by Hive and must not appear on the wire")
	}
	if strings.TrimSpace(item.Stage) != "" {
		return Issue{}, fmt.Errorf("stage is set by Hive and must not appear on the wire")
	}

	repo := strings.TrimSpace(item.Repo)
	if _, allowed := s.repos[repo]; !allowed {
		return Issue{}, fmt.Errorf("repo %q is not in work_source.external.repos", repo)
	}
	externalID := strings.TrimSpace(item.ExternalID)
	if !externalIDPattern.MatchString(externalID) {
		return Issue{}, fmt.Errorf("external_id %q must match %s", externalID, externalIDPattern.String())
	}
	if err := validateExternalItemURL(item.URL); err != nil {
		return Issue{}, err
	}

	issue := Issue{
		SourceType: s.cfg.Name,
		Repo:       repo,
		ExternalID: externalID,
		Number:     0,
		Stage:      "",
		Title:      item.Title,
		Body:       item.Body,
		Author:     item.Author,
		Labels:     item.Labels,
		Assignees:  item.Assignees,
		IsTracker:  item.IsTracker,
		Priority:   normalizeExternalPriority(item.Priority),
		State:      item.State,
		CreatedAt:  item.CreatedAt,
		UpdatedAt:  item.UpdatedAt,
		URL:        strings.TrimSpace(item.URL),
	}
	issue.DependsOn = s.dependencies(externalID, item.DependsOn)
	return issue, nil
}

// dependencies maps the wire's dependency edges. An edge that fails the repo or
// id rules is dropped while the item is kept: a blocker Hive cannot name is not
// a reason to withhold the work, and inventing a resolved edge would be worse.
func (s *externalSource) dependencies(externalID string, wire []externalWireDependency) []Dependency {
	var out []Dependency
	for _, dep := range wire {
		repo := strings.TrimSpace(dep.Repo)
		depID := strings.TrimSpace(dep.ExternalID)
		if _, allowed := s.repos[repo]; !allowed {
			s.logDroppedEdge(externalID, "repo", repo)
			continue
		}
		if !externalIDPattern.MatchString(depID) {
			s.logDroppedEdge(externalID, "external_id", depID)
			continue
		}
		out = append(out, Dependency{
			Ref:      Ref{SourceType: s.cfg.Name, Repo: repo, ExternalID: depID},
			Resolved: dep.Resolved,
		})
	}
	return out
}

func (s *externalSource) heldByLabel(labels []string) bool {
	if len(s.holds) == 0 {
		return false
	}
	for _, l := range labels {
		if _, held := s.holds[strings.TrimSpace(l)]; held {
			return true
		}
	}
	return false
}

// drop withholds an item and records why. The external_id is logged only when
// it passed its own pattern check, so a hostile value cannot inject into logs.
func (s *externalSource) drop(item externalWireItem, reason error) {
	s.dropped.Add(1)
	id := strings.TrimSpace(item.ExternalID)
	if !externalIDPattern.MatchString(id) {
		id = "(unprintable)"
	}
	s.logger().Warn("external work source dropped an item",
		"source", s.cfg.Name, "external_id", id, "reason", reason.Error())
}

func (s *externalSource) logDroppedEdge(externalID, field, value string) {
	if !externalIDPattern.MatchString(strings.TrimSpace(value)) {
		value = "(unprintable)"
	}
	s.logger().Warn("external work source dropped a dependency edge",
		"source", s.cfg.Name, "external_id", externalID, "field", field, "value", value)
}

func (s *externalSource) logger() *slog.Logger {
	if s.cfg.Logger != nil {
		return s.cfg.Logger
	}
	return slog.Default()
}

// errorf tags every adapter error with the source name, which is what the
// fail-closed log line in workSourceIssuesForCycle reports.
func (s *externalSource) errorf(format string, args ...any) error {
	return fmt.Errorf("work source %q: "+format, append([]any{s.cfg.Name}, args...)...)
}

func (s *externalSource) endpoint(path string) string {
	return strings.TrimRight(s.cfg.BaseURL, "/") + path
}

func (s *externalSource) issuesURL(cursor string) string {
	u := s.endpoint(externalIssuesPath)
	if cursor == "" {
		return u
	}
	return u + "?" + url.Values{"cursor": {cursor}}.Encode()
}

// getJSON performs one bounded GET and decodes the body.
func (s *externalSource) getJSON(ctx context.Context, rawURL string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return s.errorf("build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+s.cfg.AuthToken)
	req.Header.Set("Accept", "application/json")
	req.Header.Set(ExternalContractHeader, ExternalContract)

	resp, err := s.client.Do(req)
	if err != nil {
		return s.errorf("request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, externalMaxBodyBytes+1))
	if err != nil {
		return s.errorf("read response: %w", err)
	}
	if len(body) > externalMaxBodyBytes {
		return s.errorf("response body exceeds %d bytes", externalMaxBodyBytes)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return s.errorf("provider returned %d: %s", resp.StatusCode, s.safeBody(body))
	}
	if err := json.Unmarshal(body, out); err != nil {
		return s.errorf("decode response: %w", err)
	}
	return nil
}

// safeBody prepares a provider error body for a log line: the bearer token is
// stripped if the provider echoed it, and the rest is cut to 512 bytes.
func (s *externalSource) safeBody(body []byte) string {
	text := string(body)
	if s.cfg.AuthToken != "" {
		text = strings.ReplaceAll(text, s.cfg.AuthToken, "[redacted]")
	}
	text = strings.TrimSpace(text)
	if len(text) > externalErrorBodyLimit {
		return text[:externalErrorBodyLimit] + "…"
	}
	return text
}

// externalHTTPClient builds the client, including the private-CA bundle and the
// redirect policy that keeps the bearer token on the configured host.
func externalHTTPClient(cfg ExternalConfig) (*http.Client, error) {
	transport, _ := http.DefaultTransport.(*http.Transport)
	if transport == nil {
		return nil, fmt.Errorf("work_source.external: default HTTP transport unavailable")
	}
	cloned := transport.Clone()
	if bundle := strings.TrimSpace(cfg.CABundle); bundle != "" {
		pool, err := x509.SystemCertPool()
		if err != nil || pool == nil {
			pool = x509.NewCertPool()
		}
		if !pool.AppendCertsFromPEM([]byte(bundle)) {
			return nil, fmt.Errorf("work_source.external.ca_bundle contains no PEM certificates")
		}
		cloned.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	}
	return &http.Client{
		Transport:     cloned,
		Timeout:       cfg.Timeout,
		CheckRedirect: externalCheckRedirect,
	}, nil
}

// externalCheckRedirect refuses to follow a redirect to another host. Go would
// otherwise replay the request there, and the Authorization header is only
// dropped across hosts for a subset of cases — refusing outright is the only
// way to guarantee the provider token never leaves the configured base_url.
func externalCheckRedirect(req *http.Request, via []*http.Request) error {
	if len(via) == 0 {
		return nil
	}
	origin := via[0].URL
	if !strings.EqualFold(req.URL.Host, origin.Host) {
		return fmt.Errorf("refusing redirect from %s to %s: the provider token is never sent to another host", origin.Host, req.URL.Host)
	}
	if len(via) >= externalMaxRedirects {
		return fmt.Errorf("stopped after %d redirects", externalMaxRedirects)
	}
	return nil
}

// validateExternalItemURL enforces rule 6: an item link is https, loopback
// http, or absent. It stops a provider from making the dashboard render a
// javascript: or file: link for an operator to click.
func validateExternalItemURL(raw string) error {
	value := strings.TrimSpace(raw)
	if value == "" {
		return nil
	}
	parsed, err := url.Parse(value)
	if err != nil {
		return fmt.Errorf("url %q is not a valid URL", value)
	}
	switch parsed.Scheme {
	case "https":
		return nil
	case "http":
		if config.IsLoopbackHost(parsed.Hostname()) {
			return nil
		}
	}
	return fmt.Errorf("url %q must be https (or loopback http) or empty", value)
}

// normalizeExternalPriority clamps the wire value to the enum Hive schedules
// on. An unknown value becomes "" rather than an implicit top priority.
func normalizeExternalPriority(raw string) string {
	p := strings.ToLower(strings.TrimSpace(raw))
	if _, ok := externalPriorities[p]; ok {
		return p
	}
	return ""
}

// externalSourceResponse is the GET /v1/source body.
type externalSourceResponse struct {
	Contract    string `json:"contract"`
	SourceType  string `json:"source_type"`
	DisplayName string `json:"display_name"`
}

// externalIssuesResponse is the GET /v1/issues body.
type externalIssuesResponse struct {
	Contract   string             `json:"contract"`
	Items      []externalWireItem `json:"items"`
	NextCursor string             `json:"next_cursor"`
}

// externalWireItem mirrors the JSON tags on Issue. source_type, number, and
// stage are declared so a provider that sends them is detected and the item
// rejected, rather than the fields being silently ignored.
type externalWireItem struct {
	SourceType string                   `json:"source_type"`
	Number     int                      `json:"number"`
	Stage      string                   `json:"stage"`
	Repo       string                   `json:"repo"`
	ExternalID string                   `json:"external_id"`
	Title      string                   `json:"title"`
	Body       string                   `json:"body"`
	Author     string                   `json:"author"`
	Labels     []string                 `json:"labels"`
	Assignees  []string                 `json:"assignees"`
	IsTracker  bool                     `json:"is_tracker"`
	Priority   string                   `json:"priority"`
	State      string                   `json:"state"`
	CreatedAt  time.Time                `json:"created_at"`
	UpdatedAt  time.Time                `json:"updated_at"`
	URL        string                   `json:"url"`
	DependsOn  []externalWireDependency `json:"depends_on"`
}

// externalWireDependency is the wire shape of Dependency, which cannot be used
// directly because Dependency.Ref carries `json:"-"`.
type externalWireDependency struct {
	Repo       string `json:"repo"`
	ExternalID string `json:"external_id"`
	Resolved   bool   `json:"resolved"`
}

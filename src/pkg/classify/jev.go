package classify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
)

const (
	SourceKeywords = "keywords"
	SourceJev      = "jev"

	DecisionLane   = "lane"
	DecisionTier   = "tier"
	DecisionTriage = "triage"

	classifierBackendKeywords = "keywords"
	classifierBackendJev      = "jev"

	jevInputCostPerToken = 0.042 / 1000000.0
	jevCacheCap          = 1024
	maxJevErrBody        = 4 << 10
	disagreementRingCap  = 50
	suggestionMinSupport = 3
)

// Decider classifies one issue. Implementations may use deterministic keyword
// logic, an external typed-decision service, or a wrapper that falls back to
// keywords. The public Classify/Triage helpers route through the active decider.
type Decider interface {
	Decide(ctx context.Context, issue github.Issue, triageCfg config.TriageConfig) DecisionResult
}

type DecisionResult struct {
	Classification Classification
	Triage         *TriageDecision
}

type keywordDecider struct{}

func (keywordDecider) Decide(_ context.Context, issue github.Issue, triageCfg config.TriageConfig) DecisionResult {
	c := keywordClassification(issue)
	d := keywordTriage(issue, c, triageCfg)
	return DecisionResult{Classification: c, Triage: &d}
}

var (
	deciderMu     sync.RWMutex
	activeDecider Decider = keywordDecider{}
)

func SetDecider(d Decider) {
	deciderMu.Lock()
	defer deciderMu.Unlock()
	if d == nil {
		d = keywordDecider{}
	}
	activeDecider = d
}

func currentDecider() Decider {
	deciderMu.RLock()
	defer deciderMu.RUnlock()
	return activeDecider
}

// ConfigureDecider installs the classifier backend described by cfg. The zero
// config is keyword-only and performs no network calls.
func ConfigureDecider(cfg *config.Config) error {
	if cfg == nil {
		SetDecider(keywordDecider{})
		return nil
	}
	if err := cfg.Classifier.Validate(); err != nil {
		return err
	}
	if cfg.Classifier.EffectiveBackend() != classifierBackendJev {
		SetDecider(keywordDecider{})
		return nil
	}
	jc := cfg.Classifier.EffectiveJev()
	keyFunc := func() string {
		if jc.APIKeyEnv != "" {
			if v := strings.TrimSpace(os.Getenv(jc.APIKeyEnv)); v != "" {
				return v
			}
		}
		if gw := cfg.Governor.ResolveGateway("openrouter"); gw != nil {
			return strings.TrimSpace(gw.ResolveAPIKey())
		}
		return ""
	}
	SetDecider(newJevDecider(jc, keyFunc, http.DefaultClient))
	return nil
}

func ResetForTest() {
	SetDecider(keywordDecider{})
	ResetStats()
}

type jevDecider struct {
	cfg     config.JevClassifierConfig
	keyFunc func() string
	client  *http.Client
	cache   *jevCache
}

func newJevDecider(cfg config.JevClassifierConfig, keyFunc func() string, client *http.Client) *jevDecider {
	if client == nil {
		client = http.DefaultClient
	}
	return &jevDecider{cfg: cfg, keyFunc: keyFunc, client: client, cache: newJevCache(jevCacheCap)}
}

func (d *jevDecider) Decide(ctx context.Context, issue github.Issue, triageCfg config.TriageConfig) DecisionResult {
	kw := keywordDecider{}.Decide(ctx, issue, triageCfg)
	if !d.decisionEnabled(DecisionLane) && !d.decisionEnabled(DecisionTier) && !d.decisionEnabled(DecisionTriage) {
		recordFallback(DecisionLane)
		recordFallback(DecisionTier)
		recordFallback(DecisionTriage)
		return kw
	}
	key := issueCacheKey(issue)
	if cached, ok := d.cache.get(key); ok {
		return d.apply(issue, triageCfg, kw, cached)
	}
	apiKey := ""
	if d.keyFunc != nil {
		apiKey = strings.TrimSpace(d.keyFunc())
	}
	if apiKey == "" {
		recordFallbacks(d.cfg.EffectiveDecisions())
		return kw
	}
	ctx, cancel := context.WithTimeout(ctx, d.cfg.EffectiveTimeout())
	defer cancel()
	out, err := d.call(ctx, issue, apiKey)
	if err != nil {
		recordFallbacks(d.cfg.EffectiveDecisions())
		return kw
	}
	d.cache.add(key, out)
	recordUsage(out.InputTokens)
	return d.apply(issue, triageCfg, kw, out)
}

func (d *jevDecider) decisionEnabled(name string) bool {
	for _, v := range d.cfg.EffectiveDecisions() {
		if v == name {
			return true
		}
	}
	return false
}

func (d *jevDecider) apply(issue github.Issue, triageCfg config.TriageConfig, kw DecisionResult, out jevOutcome) DecisionResult {
	res := kw
	minConfidence := d.cfg.EffectiveMinConfidence()
	if d.decisionEnabled(DecisionLane) {
		if out.Lane.Confidence >= minConfidence && out.Lane.Value != "" {
			recordAgreement(DecisionLane, string(kw.Classification.Lane) == out.Lane.Value, disagreementRecordForIssue(issue, DecisionLane, string(kw.Classification.Lane), out.Lane.Value, out.Lane.Confidence))
		} else {
			recordFallback(DecisionLane)
		}
	}
	if d.decisionEnabled(DecisionTier) {
		if out.Tier.Confidence >= minConfidence && out.Tier.Value != "" {
			recordAgreement(DecisionTier, string(kw.Classification.Tier) == out.Tier.Value, disagreementRecordForIssue(issue, DecisionTier, string(kw.Classification.Tier), out.Tier.Value, out.Tier.Confidence))
		} else {
			recordFallback(DecisionTier)
		}
	}
	if d.decisionEnabled(DecisionTriage) {
		if out.Triage.Confidence >= minConfidence && out.Triage.Value != "" {
			if kw.Triage != nil {
				recordAgreement(DecisionTriage, string(kw.Triage.Verdict) == out.Triage.Value, disagreementRecordForIssue(issue, DecisionTriage, string(kw.Triage.Verdict), out.Triage.Value, out.Triage.Confidence))
			}
		} else {
			recordFallback(DecisionTriage)
		}
	}
	decision := keywordTriage(issue, res.Classification, triageCfg)
	res.Triage = &decision
	return res
}

type jevRequest struct {
	State     any                    `json:"state"`
	Model     string                 `json:"model"`
	Questions map[string]jevQuestion `json:"questions"`
}

type jevQuestion struct {
	Type         string `json:"type"`
	Instructions any    `json:"instructions"`
	Criteria     any    `json:"criteria"`
}

type jevResponse struct {
	Model   string               `json:"model"`
	Answers map[string]jevAnswer `json:"answers"`
	Usage   struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
}

type jevAnswer struct {
	Type          string             `json:"type"`
	Choice        string             `json:"choice"`
	Confidence    float64            `json:"confidence"`
	Probabilities map[string]float64 `json:"probabilities"`
}

type jevOutcome struct {
	Lane        jevChoice
	Tier        jevChoice
	Triage      jevChoice
	InputTokens int
}

type jevChoice struct {
	Value      string
	Confidence float64
}

func (d *jevDecider) call(ctx context.Context, issue github.Issue, apiKey string) (jevOutcome, error) {
	reqBody := jevRequest{State: jevState(issue), Model: d.cfg.EffectiveModel(), Questions: map[string]jevQuestion{}}
	if d.decisionEnabled(DecisionLane) {
		reqBody.Questions[DecisionLane] = jevQuestion{Type: "choice", Instructions: "Choose the best Hive agent lane for this issue.", Criteria: laneCriteria()}
	}
	if d.decisionEnabled(DecisionTier) {
		reqBody.Questions[DecisionTier] = jevQuestion{Type: "choice", Instructions: "Choose the implementation complexity tier for this issue.", Criteria: map[string]string{string(TierSimple): "Small localized documentation, label, copy, accessibility, cosmetic, or test-only cleanup.", string(TierMedium): "Ordinary implementation work with limited cross-cutting risk.", string(TierComplex): "Architecture, migration, security, regression, race, performance, API, or high-risk change."}}
	}
	if d.decisionEnabled(DecisionTriage) {
		reqBody.Questions[DecisionTriage] = jevQuestion{Type: "choice", Instructions: "Choose the run triage verdict. Use clarify when the issue lacks enough detail or still contains templates/options; spec when design/specification should happen before coding; fix when implementation can proceed directly.", Criteria: map[string]string{string(TriageFix): "Ready for direct implementation.", string(TriageSpec): "Needs a specification/design run before implementation.", string(TriageClarify): "Needs human clarification before work should start."}}
	}
	body, err := json.Marshal(reqBody)
	if err != nil {
		return jevOutcome{}, err
	}
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, d.cfg.EffectiveEndpoint(), bytes.NewReader(body))
	if err != nil {
		return jevOutcome{}, err
	}
	hreq.Header.Set("Content-Type", "application/json")
	hreq.Header.Set("Authorization", "Bearer "+apiKey)
	resp, err := d.client.Do(hreq)
	if err != nil {
		return jevOutcome{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, maxJevErrBody))
		return jevOutcome{}, fmt.Errorf("jev returned HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	var decoded jevResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxJevErrBody)).Decode(&decoded); err != nil {
		return jevOutcome{}, err
	}
	out := jevOutcome{InputTokens: decoded.Usage.InputTokens}
	out.Lane = answerChoice(decoded.Answers[DecisionLane])
	out.Tier = answerChoice(decoded.Answers[DecisionTier])
	out.Triage = answerChoice(decoded.Answers[DecisionTriage])
	if out.Lane.Value != "" && !validLane(out.Lane.Value) {
		out.Lane = jevChoice{}
	}
	if out.Tier.Value != string(TierSimple) && out.Tier.Value != string(TierMedium) && out.Tier.Value != string(TierComplex) {
		out.Tier = jevChoice{}
	}
	if out.Triage.Value != string(TriageFix) && out.Triage.Value != string(TriageSpec) && out.Triage.Value != string(TriageClarify) {
		out.Triage = jevChoice{}
	}
	return out, nil
}

func answerChoice(a jevAnswer) jevChoice {
	conf := a.Confidence
	if conf == 0 && a.Choice != "" && a.Probabilities != nil {
		conf = a.Probabilities[a.Choice]
	}
	return jevChoice{Value: strings.TrimSpace(a.Choice), Confidence: conf}
}

func jevState(issue github.Issue) map[string]any {
	return map[string]any{"repo": issue.Repo, "number": issue.Number, "title": issue.Title, "body": issue.Body, "labels": issue.Labels, "author": issue.Author, "updated_at": issue.UpdatedAt.Format(time.RFC3339)}
}

func laneCriteria() map[string]string {
	criteria := map[string]string{string(LaneScanner): "Default implementation/scanner lane for uncategorized fixable work."}
	for _, lane := range activeLanes() {
		criteria[lane.Name] = "Issue matching lane keywords: " + strings.Join(lane.Keywords, ", ")
	}
	return criteria
}

func validLane(v string) bool {
	if v == string(LaneScanner) {
		return true
	}
	for _, lane := range activeLanes() {
		if lane.Name == v {
			return true
		}
	}
	return false
}

func issueCacheKey(issue github.Issue) string {
	return fmt.Sprintf("%s#%d@%s", issue.Repo, issue.Number, issue.UpdatedAt.UTC().Format(time.RFC3339Nano))
}

type jevCache struct {
	mu    sync.Mutex
	cap   int
	order []string
	data  map[string]jevOutcome
}

func newJevCache(capacity int) *jevCache {
	return &jevCache{cap: capacity, data: map[string]jevOutcome{}}
}
func (c *jevCache) get(key string) (jevOutcome, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	v, ok := c.data[key]
	return v, ok
}
func (c *jevCache) add(key string, value jevOutcome) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.data[key]; ok {
		c.data[key] = value
		return
	}
	if len(c.order) >= c.cap && len(c.order) > 0 {
		old := c.order[0]
		c.order = c.order[1:]
		delete(c.data, old)
	}
	c.order = append(c.order, key)
	c.data[key] = value
}

type Stats struct {
	Decisions            map[string]DecisionStats  `json:"decisions"`
	Disagreements        map[string][]Disagreement `json:"disagreements,omitempty"`
	RuleSuggestions      []RuleSuggestion          `json:"rule_suggestions,omitempty"`
	EstimatedInputTokens int64                     `json:"estimated_input_tokens"`
	EstimatedSpendUSD    float64                   `json:"estimated_spend_usd"`
}
type DecisionStats struct {
	Agree    int64 `json:"agree"`
	Disagree int64 `json:"disagree"`
	Fallback int64 `json:"fallback"`
}
type Disagreement struct {
	Repo          string  `json:"repo"`
	Number        int     `json:"number"`
	Title         string  `json:"title"`
	KeywordAnswer string  `json:"keyword_answer"`
	JevAnswer     string  `json:"jev_answer"`
	Confidence    float64 `json:"confidence"`
}
type RuleSuggestion struct {
	ID       string   `json:"id"`
	Decision string   `json:"decision"`
	Answer   string   `json:"answer"`
	Keyword  string   `json:"keyword"`
	Target   string   `json:"target"`
	Support  int      `json:"support"`
	Examples []string `json:"examples,omitempty"`
}

var statsMu sync.Mutex
var stats = Stats{Decisions: map[string]DecisionStats{}, Disagreements: map[string][]Disagreement{}}

func ResetStats() {
	statsMu.Lock()
	defer statsMu.Unlock()
	stats = Stats{Decisions: map[string]DecisionStats{}, Disagreements: map[string][]Disagreement{}}
}
func CurrentStats() Stats {
	statsMu.Lock()
	defer statsMu.Unlock()
	out := Stats{Decisions: map[string]DecisionStats{}, Disagreements: map[string][]Disagreement{}, EstimatedInputTokens: stats.EstimatedInputTokens, EstimatedSpendUSD: stats.EstimatedSpendUSD}
	for k, v := range stats.Decisions {
		out.Decisions[k] = v
	}
	for k, v := range stats.Disagreements {
		out.Disagreements[k] = append([]Disagreement(nil), v...)
	}
	out.RuleSuggestions = buildRuleSuggestions(out.Disagreements)
	return out
}
func recordAgreement(decision string, agree bool, disagreement Disagreement) {
	statsMu.Lock()
	defer statsMu.Unlock()
	ds := stats.Decisions[decision]
	if agree {
		ds.Agree++
	} else {
		ds.Disagree++
		recordDisagreementLocked(decision, disagreement)
	}
	stats.Decisions[decision] = ds
}
func recordFallback(decision string) {
	statsMu.Lock()
	defer statsMu.Unlock()
	ds := stats.Decisions[decision]
	ds.Fallback++
	stats.Decisions[decision] = ds
}
func recordFallbacks(decisions []string) {
	for _, d := range decisions {
		recordFallback(d)
	}
}
func recordUsage(tokens int) {
	if tokens <= 0 {
		return
	}
	statsMu.Lock()
	defer statsMu.Unlock()
	stats.EstimatedInputTokens += int64(tokens)
	stats.EstimatedSpendUSD += float64(tokens) * jevInputCostPerToken
}

func disagreementRecordForIssue(issue github.Issue, decision, keywordAnswer, jevAnswer string, confidence float64) Disagreement {
	return Disagreement{Repo: issue.Repo, Number: issue.Number, Title: issue.Title, KeywordAnswer: keywordAnswer, JevAnswer: jevAnswer, Confidence: confidence}
}

func recordDisagreementLocked(decision string, disagreement Disagreement) {
	if disagreement.JevAnswer == "" {
		return
	}
	ring := stats.Disagreements[decision]
	for i := range ring {
		if ring[i].Repo == disagreement.Repo && ring[i].Number == disagreement.Number {
			ring[i] = disagreement
			stats.Disagreements[decision] = ring
			return
		}
	}
	if len(ring) >= disagreementRingCap {
		ring = ring[1:]
	}
	ring = append(ring, disagreement)
	stats.Disagreements[decision] = ring
}

func buildRuleSuggestions(disagreements map[string][]Disagreement) []RuleSuggestion {
	var suggestions []RuleSuggestion
	suggestions = append(suggestions, buildLaneSuggestions(disagreements[DecisionLane])...)
	suggestions = append(suggestions, buildTierSuggestions(disagreements[DecisionTier])...)
	return suggestions
}

func buildLaneSuggestions(disagreements []Disagreement) []RuleSuggestion {
	existingByLane := map[string]map[string]bool{}
	for _, lane := range activeLanes() {
		existingByLane[lane.Name] = keywordSet(lane.Keywords)
	}
	return buildSuggestionsForTarget(DecisionLane, disagreements, func(answer string) (string, map[string]bool, bool) {
		if answer == string(LaneScanner) {
			return "", nil, false
		}
		existing, ok := existingByLane[answer]
		if !ok {
			return "", nil, false
		}
		return "agents." + answer + ".lane_keywords", existing, true
	})
}

func buildTierSuggestions(disagreements []Disagreement) []RuleSuggestion {
	simple, complex := TierKeywords()
	return buildSuggestionsForTarget(DecisionTier, disagreements, func(answer string) (string, map[string]bool, bool) {
		switch answer {
		case string(TierSimple):
			return "classifier.simple_keywords", keywordSet(simple), true
		case string(TierComplex):
			return "classifier.complex_signals", keywordSet(complex), true
		default:
			return "", nil, false
		}
	})
}

func buildSuggestionsForTarget(decision string, disagreements []Disagreement, target func(string) (string, map[string]bool, bool)) []RuleSuggestion {
	type bucket struct {
		counts   map[string]int
		examples map[string][]string
		target   string
		existing map[string]bool
	}
	buckets := map[string]*bucket{}
	for _, d := range disagreements {
		targetName, existing, ok := target(d.JevAnswer)
		if !ok {
			continue
		}
		key := d.JevAnswer
		b := buckets[key]
		if b == nil {
			b = &bucket{counts: map[string]int{}, examples: map[string][]string{}, target: targetName, existing: existing}
			buckets[key] = b
		}
		for _, token := range titleTokens(d.Title) {
			if b.existing[token] {
				continue
			}
			b.counts[token]++
			if len(b.examples[token]) < 3 {
				b.examples[token] = append(b.examples[token], fmt.Sprintf("%s#%d %s", d.Repo, d.Number, d.Title))
			}
		}
	}
	var suggestions []RuleSuggestion
	for answer, b := range buckets {
		for token, support := range b.counts {
			if support < suggestionMinSupport {
				continue
			}
			suggestions = append(suggestions, RuleSuggestion{ID: suggestionID(decision, answer, token), Decision: decision, Answer: answer, Keyword: token, Target: b.target, Support: support, Examples: append([]string(nil), b.examples[token]...)})
		}
	}
	sortRuleSuggestions(suggestions)
	return suggestions
}

func keywordSet(in []string) map[string]bool {
	out := map[string]bool{}
	for _, v := range in {
		if s := strings.ToLower(strings.TrimSpace(v)); s != "" {
			out[s] = true
		}
	}
	return out
}

func suggestionID(decision, answer, keyword string) string {
	return decision + ":" + answer + ":" + keyword
}

func sortRuleSuggestions(suggestions []RuleSuggestion) {
	sort.Slice(suggestions, func(i, j int) bool {
		if suggestions[i].Support != suggestions[j].Support {
			return suggestions[i].Support > suggestions[j].Support
		}
		if suggestions[i].Decision != suggestions[j].Decision {
			return suggestions[i].Decision < suggestions[j].Decision
		}
		if suggestions[i].Answer != suggestions[j].Answer {
			return suggestions[i].Answer < suggestions[j].Answer
		}
		return suggestions[i].Keyword < suggestions[j].Keyword
	})
}

func titleTokens(title string) []string {
	fields := strings.FieldsFunc(strings.ToLower(title), func(r rune) bool {
		return !(r >= 'a' && r <= 'z') && !(r >= '0' && r <= '9')
	})
	seen := map[string]bool{}
	var out []string
	for _, f := range fields {
		f = strings.TrimSpace(f)
		if len(f) < 3 || titleStopwords[f] || seen[f] {
			continue
		}
		seen[f] = true
		out = append(out, f)
	}
	return out
}

var titleStopwords = map[string]bool{
	"the": true, "and": true, "for": true, "with": true, "from": true, "into": true,
	"this": true, "that": true, "these": true, "those": true, "when": true, "where": true,
	"what": true, "why": true, "how": true, "should": true, "could": true, "would": true,
	"must": true, "can": true, "not": true, "fix": true, "add": true, "update": true,
	"remove": true, "make": true, "use": true, "new": true, "old": true, "issue": true,
	"bug": true, "task": true, "hive": true, "dashboard": true,
}

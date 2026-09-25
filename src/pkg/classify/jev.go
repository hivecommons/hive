package classify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
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
	classifierModeShadow      = "shadow"
	classifierModeEnforce     = "enforce"

	jevInputCostPerToken = 0.042 / 1000000.0
	jevCacheCap          = 1024
	maxJevErrBody        = 4 << 10
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
	mode := d.cfg.EffectiveMode()
	minConfidence := d.cfg.EffectiveMinConfidence()
	classSource := SourceKeywords
	classConfidence := 0.0
	triageOverridden := false
	if d.decisionEnabled(DecisionLane) {
		if out.Lane.Confidence >= minConfidence && out.Lane.Value != "" {
			if mode == classifierModeEnforce {
				res.Classification.Lane = Lane(out.Lane.Value)
				classSource = SourceJev
				classConfidence = maxFloat(classConfidence, out.Lane.Confidence)
			}
			recordAgreement(DecisionLane, string(kw.Classification.Lane) == out.Lane.Value)
		} else {
			recordFallback(DecisionLane)
		}
	}
	if d.decisionEnabled(DecisionTier) {
		if out.Tier.Confidence >= minConfidence && out.Tier.Value != "" {
			if mode == classifierModeEnforce {
				res.Classification.Tier = Tier(out.Tier.Value)
				res.Classification.Model = tierToModel(res.Classification.Tier)
				classSource = SourceJev
				classConfidence = maxFloat(classConfidence, out.Tier.Confidence)
			}
			recordAgreement(DecisionTier, string(kw.Classification.Tier) == out.Tier.Value)
		} else {
			recordFallback(DecisionTier)
		}
	}
	if classSource == SourceJev {
		res.Classification.Source = SourceJev
		res.Classification.Confidence = classConfidence
	}
	if d.decisionEnabled(DecisionTriage) {
		if out.Triage.Confidence >= minConfidence && out.Triage.Value != "" {
			if mode == classifierModeEnforce {
				decision := triageDecision(TriageVerdict(out.Triage.Value), "Jev typed-decision backend", "jev:confidence")
				res.Triage = &decision
				triageOverridden = true
			}
			if kw.Triage != nil {
				recordAgreement(DecisionTriage, string(kw.Triage.Verdict) == out.Triage.Value)
			}
		} else {
			recordFallback(DecisionTriage)
		}
	}
	if !triageOverridden {
		decision := keywordTriage(issue, res.Classification, triageCfg)
		res.Triage = &decision
	}
	return res
}

func maxFloat(a, b float64) float64 {
	if b > a {
		return b
	}
	return a
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
	Decisions            map[string]DecisionStats `json:"decisions"`
	EstimatedInputTokens int64                    `json:"estimated_input_tokens"`
	EstimatedSpendUSD    float64                  `json:"estimated_spend_usd"`
}
type DecisionStats struct {
	Agree    int64 `json:"agree"`
	Disagree int64 `json:"disagree"`
	Fallback int64 `json:"fallback"`
}

var statsMu sync.Mutex
var stats = Stats{Decisions: map[string]DecisionStats{}}

func ResetStats() {
	statsMu.Lock()
	defer statsMu.Unlock()
	stats = Stats{Decisions: map[string]DecisionStats{}}
}
func CurrentStats() Stats {
	statsMu.Lock()
	defer statsMu.Unlock()
	out := Stats{Decisions: map[string]DecisionStats{}, EstimatedInputTokens: stats.EstimatedInputTokens, EstimatedSpendUSD: stats.EstimatedSpendUSD}
	for k, v := range stats.Decisions {
		out.Decisions[k] = v
	}
	return out
}
func recordAgreement(decision string, agree bool) {
	statsMu.Lock()
	defer statsMu.Unlock()
	ds := stats.Decisions[decision]
	if agree {
		ds.Agree++
	} else {
		ds.Disagree++
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

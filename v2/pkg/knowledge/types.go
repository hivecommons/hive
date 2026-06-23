package knowledge

import "time"

// LayerType identifies the scope and privacy of a knowledge wiki layer.
type LayerType string

const (
	LayerPersonal  LayerType = "personal"
	LayerProject   LayerType = "project"
	LayerOrg       LayerType = "org"
	LayerCommunity LayerType = "community"
)

// Precedence returns the merge priority (lower = higher priority).
// Personal overrides everything; community is the fallback.
func (l LayerType) Precedence() int {
	switch l {
	case LayerPersonal:
		return 1
	case LayerProject:
		return 2
	case LayerOrg:
		return 3
	case LayerCommunity:
		return 4
	default:
		return 99
	}
}

// LayerConfig describes a single wiki layer in hive.yaml.
type LayerConfig struct {
	Type   LayerType `yaml:"type"   json:"type"`
	Path   string    `yaml:"path"   json:"path,omitempty"`
	URL    string    `yaml:"url"    json:"url,omitempty"`
	Shared bool      `yaml:"shared" json:"shared"`
}

// Endpoint returns the HTTP URL for this layer. Local layers use path-based
// access; remote layers use their configured URL.
func (l LayerConfig) Endpoint() string {
	if l.URL != "" {
		return l.URL
	}
	return ""
}

// KnowledgeConfig is the top-level knowledge section of hive.yaml.
type KnowledgeConfig struct {
	Enabled         bool                  `yaml:"enabled"          json:"enabled"`
	Engine          string                `yaml:"engine"           json:"engine"`
	Layers          []LayerConfig         `yaml:"layers"           json:"layers"`
	GitSources      []GitSourceConfig     `yaml:"git_sources"      json:"git_sources,omitempty"`
	Curator         CuratorConfig         `yaml:"curator"          json:"curator"`
	Primer          PrimerConfig          `yaml:"primer"           json:"primer"`
	BeadSynthesizer BeadSynthesizerConfig `yaml:"bead_synthesizer" json:"bead_synthesizer"`
}

// BeadSynthesizerConfig controls automatic synthesis of completed beads into wiki facts.
// Enabled defaults to true when knowledge is enabled; set to false to opt out.
type BeadSynthesizerConfig struct {
	Enabled          *bool   `yaml:"enabled,omitempty"   json:"enabled"`
	Schedule         string  `yaml:"schedule"            json:"schedule"`
	MinConfidence    float64 `yaml:"min_confidence"      json:"min_confidence"`
	TargetLayer      string  `yaml:"target_layer"        json:"target_layer"`
	MaxFactsPerCycle int     `yaml:"max_facts_per_cycle" json:"max_facts_per_cycle"`
	VaultPath        string  `yaml:"vault_path"          json:"vault_path"`
}

// IsEnabled returns whether bead synthesis is enabled (defaults to true).
func (b BeadSynthesizerConfig) IsEnabled() bool {
	if b.Enabled == nil {
		return true
	}
	return *b.Enabled
}

// CuratorConfig controls automated knowledge extraction from merged PRs.
type CuratorConfig struct {
	Schedule              string   `yaml:"schedule"                json:"schedule"`
	ExtractFrom           []string `yaml:"extract_from"            json:"extract_from"`
	AutoPromoteThreshold  float64  `yaml:"auto_promote_threshold"  json:"auto_promote_threshold"`
}

// PrimerConfig controls how facts are selected and injected into agent kicks.
type PrimerConfig struct {
	MaxFacts      int      `yaml:"max_facts"       json:"max_facts"`
	Priority      []string `yaml:"priority"        json:"priority"`
	MergeStrategy string   `yaml:"merge_strategy"  json:"merge_strategy"`
}

// FactType categorizes knowledge entries.
type FactType string

const (
	// Operational fact types (L2+ — post-project)
	FactPattern     FactType = "pattern"
	FactGotcha      FactType = "gotcha"
	FactDecision    FactType = "decision"
	FactRegression  FactType = "regression"
	FactTestScaff   FactType = "test_scaffold"
	FactIntegration FactType = "integration"
	FactCoverage    FactType = "coverage_rule"

	// Ideation fact types (L1 — project inception)
	FactIdea         FactType = "idea"
	FactVision       FactType = "vision"
	FactConstitution FactType = "constitution"
	FactRequirement  FactType = "requirement"
	FactConstraint   FactType = "constraint"
	FactStakeholder  FactType = "stakeholder"
	FactAcceptance   FactType = "acceptance"
)

// FactPhase groups fact types by lifecycle stage.
type FactPhase string

const (
	PhaseIdeation    FactPhase = "ideation"
	PhaseDevelopment FactPhase = "development"
	PhaseOperational FactPhase = "operational"
)

// Phase returns the lifecycle phase of a fact type.
func (ft FactType) Phase() FactPhase {
	switch ft {
	case FactIdea, FactVision, FactConstitution, FactRequirement,
		FactConstraint, FactStakeholder, FactAcceptance:
		return PhaseIdeation
	default:
		return PhaseOperational
	}
}

// IsIdeation returns true for fact types produced during L1 inception.
func (ft FactType) IsIdeation() bool {
	return ft.Phase() == PhaseIdeation
}

// Fact is a single knowledge entry returned by the wiki.
type Fact struct {
	Slug       string    `json:"slug"`
	Title      string    `json:"title"`
	Type       FactType  `json:"type"`
	Body       string    `json:"body"`
	Confidence float64   `json:"confidence"`
	Status     string    `json:"status"`
	Tags       []string  `json:"tags"`
	Layer      LayerType `json:"layer"`
	Sources    []Source  `json:"sources,omitempty"`
	Related    []string  `json:"related,omitempty"`
	UsageCount int       `json:"usage_count"`
	LastUsed   time.Time `json:"last_used,omitempty"`

	// Supersedes links to the fact this one replaced during L1→L2 evolution
	// (e.g., acceptance → test_scaffold).
	Supersedes string    `json:"supersedes,omitempty"`
	Phase      FactPhase `json:"phase,omitempty"`
}

// Source tracks where a fact was extracted from.
type Source struct {
	PR      string    `json:"pr,omitempty"`
	Comment string    `json:"comment,omitempty"`
	Author  string    `json:"author,omitempty"`
	Date    time.Time `json:"date"`
}

// PrimedKnowledge is the result of priming — ready to inject into an agent kick.
type PrimedKnowledge struct {
	Facts     []Fact `json:"facts"`
	QueryTime int64  `json:"query_time_ms"`
}

// FormatForPrompt renders primed facts as markdown for injection into an agent's
// kick prompt. This runs once during kick preparation; the agent never queries
// the wiki directly.
func (pk *PrimedKnowledge) FormatForPrompt() string {
	if len(pk.Facts) == 0 {
		return ""
	}

	var b []byte
	b = append(b, "# Relevant Knowledge\n\n"...)

	typeSections := map[string][]Fact{}
	typeOrder := []string{}
	for _, f := range pk.Facts {
		key := string(f.Type)
		if _, exists := typeSections[key]; !exists {
			typeOrder = append(typeOrder, key)
		}
		typeSections[key] = append(typeSections[key], f)
	}

	for _, typ := range typeOrder {
		facts := typeSections[typ]
		b = append(b, "## "+typ+"\n\n"...)
		for _, f := range facts {
			b = append(b, "- **"+f.Title+"**"...)
			if f.Confidence < 1.0 {
				b = append(b, " (confidence: "...)
				b = append(b, formatConfidence(f.Confidence)...)
				b = append(b, ")"...)
			}
			b = append(b, "\n  "...)
			b = append(b, f.Body...)
			b = append(b, "\n\n"...)
		}
	}

	return string(b)
}

// InceptionMode distinguishes greenfield (new project) from brownfield (existing repo).
type InceptionMode string

const (
	InceptionGreenfield InceptionMode = "greenfield"
	InceptionBrownfield InceptionMode = "brownfield"
)

// InceptionPhase tracks progression through the inception workflow.
type InceptionPhase string

const (
	PhaseCapture   InceptionPhase = "capture"
	PhaseClarify   InceptionPhase = "clarify"
	PhaseStructure InceptionPhase = "structure"
	PhaseScaffold  InceptionPhase = "scaffold"
	PhaseComplete  InceptionPhase = "complete"
)

// Question is a clarification question generated by the guide agent during inception.
type Question struct {
	ID       string `json:"id"`
	Text     string `json:"text"`
	Default  string `json:"default,omitempty"`
	Category string `json:"category"` // "language", "users", "features", "constraints", "testing"
}

// InceptionState tracks the progress of a Level 1 ideation workflow.
type InceptionState struct {
	Phase          InceptionPhase    `json:"phase"`
	Mode           InceptionMode     `json:"mode"`
	IdeaText       string            `json:"idea_text"`
	IdeaSlug       string            `json:"idea_slug"`
	RepoURL        string            `json:"repo_url,omitempty"`
	Questions      []Question        `json:"questions"`
	Answers        map[string]string `json:"answers"`
	FactSlugs      []string          `json:"fact_slugs"`
	StartedAt      time.Time         `json:"started_at"`
	PhaseChangedAt *time.Time        `json:"phase_changed_at,omitempty"`
	WikiName       string            `json:"wiki_name,omitempty"`
	AutoFactCount     int            `json:"auto_fact_count,omitempty"`
	AutoQuestionCount int            `json:"auto_question_count,omitempty"`
}

// ScaffoldFile is a single generated file in the scaffold output.
type ScaffoldFile struct {
	Path    string `json:"path"`
	Content string `json:"content"`
	Purpose string `json:"purpose"` // "readme", "claude_md", "test_stub", "ci", "contributing"
	IsNew   bool   `json:"is_new"`  // false for brownfield amendments to existing files
}

// ScaffoldResult holds all generated files from inception.
type ScaffoldResult struct {
	Files []ScaffoldFile `json:"files"`
}

// EvolutionRule maps an ideation fact type to its operational successor.
type EvolutionRule struct {
	From    FactType
	To      FactType
	Trigger string // human-readable description of what triggers evolution
}

// EvolutionRules defines how ideation facts evolve into operational facts at L2+.
var EvolutionRules = []EvolutionRule{
	{FactAcceptance, FactTestScaff, "test stubs filled with real assertions"},
	{FactConstraint, FactGotcha, "constraint validated by a real failure"},
	{FactConstitution, FactDecision, "principles become project decisions with provenance"},
}

func formatConfidence(c float64) string {
	pct := int(c * 100)
	switch {
	case pct >= 90:
		return "high"
	case pct >= 70:
		return "medium"
	default:
		return "low"
	}
}

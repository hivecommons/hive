package knowledge

import (
	"strings"
	"time"
)

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

// DocSourceConfig describes an external document (PDF, URL, text file, or
// Context7 library) to import as knowledge facts.
type DocSourceConfig struct {
	Name       string    `yaml:"name"        json:"name"`
	URL        string    `yaml:"url"         json:"url,omitempty"`
	FilePath   string    `yaml:"file_path"   json:"file_path,omitempty"`
	Context7ID string    `yaml:"context7_id" json:"context7_id,omitempty"`
	Layer      LayerType `yaml:"layer"       json:"layer"`
}

// Context7AutoImport describes a library to auto-import from Context7 at startup.
type Context7AutoImport struct {
	ID    string `yaml:"id"    json:"id"`
	Query string `yaml:"query" json:"query,omitempty"`
}

// Context7Config configures automatic Context7 library documentation imports.
type Context7Config struct {
	AutoImport []Context7AutoImport `yaml:"auto_import" json:"auto_import,omitempty"`
}

// KnowledgeConfig is the top-level knowledge section of hive.yaml.
type KnowledgeConfig struct {
	Enabled         bool                  `yaml:"enabled"          json:"enabled"`
	Engine          string                `yaml:"engine"           json:"engine"`
	Layers          []LayerConfig         `yaml:"layers"           json:"layers"`
	GitSources      []GitSourceConfig     `yaml:"git_sources"      json:"git_sources,omitempty"`
	Documents       []DocSourceConfig     `yaml:"documents"        json:"documents,omitempty"`
	Context7        Context7Config        `yaml:"context7"         json:"context7,omitempty"`
	Curator         CuratorConfig         `yaml:"curator"          json:"curator"`
	Primer          PrimerConfig          `yaml:"primer"           json:"primer"`
	BeadSynthesizer BeadSynthesizerConfig `yaml:"bead_synthesizer" json:"bead_synthesizer"`
}

// BeadSynthesizerConfig controls automatic synthesis of completed beads into wiki facts.
// Enabled defaults to true when knowledge is enabled; set to false to opt out.
type BeadSynthesizerConfig struct {
	Enabled          *bool            `yaml:"enabled,omitempty"    json:"enabled"`
	Schedule         string           `yaml:"schedule"             json:"schedule"`
	MinConfidence    float64          `yaml:"min_confidence"       json:"min_confidence"`
	TargetLayer      string           `yaml:"target_layer"         json:"target_layer"`
	MaxFactsPerCycle int              `yaml:"max_facts_per_cycle"  json:"max_facts_per_cycle"`
	VaultPath        string           `yaml:"vault_path"           json:"vault_path"`
	Org              string           `yaml:"org"                  json:"org"`
	Repos            []string         `yaml:"repos"                json:"repos"`
	RetentionPolicy  *RetentionPolicy `yaml:"retention_policy"     json:"retention_policy,omitempty"`
}

// RetentionPolicy controls intelligent bead lifecycle management.
type RetentionPolicy struct {
	MaxBeads               int  `yaml:"max_beads"                json:"max_beads"`
	ArchiveAfterSynthDays  int  `yaml:"archive_after_synth_days" json:"archive_after_synth_days"`
	HighPriorityRetainDays int  `yaml:"high_priority_retain_days" json:"high_priority_retain_days"`
	PreserveWithDeps       bool `yaml:"preserve_with_deps"       json:"preserve_with_deps"`
}

// IsEnabled returns whether bead synthesis is enabled (defaults to true).
func (b BeadSynthesizerConfig) IsEnabled() bool {
	if b.Enabled == nil {
		return true
	}
	return *b.Enabled
}

// CuratorConfig controls promotion of verified knowledge facts between layers.
type CuratorConfig struct {
	AutoPromoteThreshold float64 `yaml:"auto_promote_threshold" json:"auto_promote_threshold"`
}

// ExtractedFact is a fact candidate ready to ingest into a knowledge layer.
type ExtractedFact struct {
	Title      string    `json:"title"`
	Body       string    `json:"body"`
	Type       FactType  `json:"type"`
	Confidence float64   `json:"confidence"`
	Tags       []string  `json:"tags"`
	Related    []string  `json:"related,omitempty"`
	SourcePR   string    `json:"source_pr"`
	SourceDate time.Time `json:"source_date"`
}

func containsAny(s string, needles ...string) bool {
	for _, n := range needles {
		if strings.Contains(s, n) {
			return true
		}
	}
	return false
}

func extractTags(comment string) []string {
	lower := strings.ToLower(comment)
	var tags []string
	tagKeywords := map[string]string{
		"typescript": "typescript",
		"react":      "react",
		"go ":        "go",
		"golang":     "go",
		"test":       "testing",
		"ci":         "ci",
		"docker":     "docker",
		"kubernetes": "kubernetes",
		"k8s":        "kubernetes",
		"helm":       "helm",
		"security":   "security",
		"auth":       "auth",
	}
	seen := make(map[string]bool)
	for keyword, tag := range tagKeywords {
		if strings.Contains(lower, keyword) && !seen[tag] {
			tags = append(tags, tag)
			seen[tag] = true
		}
	}
	return tags
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
	FactReference   FactType = "reference"

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
	DocURL  string    `json:"doc_url,omitempty"`
	DocSlug string    `json:"doc_slug,omitempty"`
}

// PrimedKnowledge is the result of priming — ready to inject into an agent kick.
type PrimedKnowledge struct {
	Facts     []Fact   `json:"facts"`
	Hints     []string `json:"hints,omitempty"`
	QueryTime int64    `json:"query_time_ms"`
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
			if docURL := factDocURL(f); docURL != "" {
				b = append(b, " (source: "...)
				b = append(b, docURL...)
				b = append(b, ")"...)
			}
			b = append(b, "\n  "...)
			b = append(b, f.Body...)
			b = append(b, "\n\n"...)
		}
	}

	if len(pk.Hints) > 0 {
		b = append(b, "## Available Documentation\n\n"...)
		for _, hint := range pk.Hints {
			b = append(b, "- "...)
			b = append(b, hint...)
			b = append(b, "\n"...)
		}
		b = append(b, "\n"...)
	}

	return string(b)
}

func factDocURL(f Fact) string {
	for _, s := range f.Sources {
		if s.DocURL != "" {
			return s.DocURL
		}
	}
	return ""
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
	Phase             InceptionPhase    `json:"phase"`
	Mode              InceptionMode     `json:"mode"`
	IdeaText          string            `json:"idea_text"`
	IdeaSlug          string            `json:"idea_slug"`
	RepoURL           string            `json:"repo_url,omitempty"`
	Questions         []Question        `json:"questions"`
	Answers           map[string]string `json:"answers"`
	FactSlugs         []string          `json:"fact_slugs"`
	StartedAt         time.Time         `json:"started_at"`
	PhaseChangedAt    *time.Time        `json:"phase_changed_at,omitempty"`
	WikiName          string            `json:"wiki_name,omitempty"`
	AutoFactCount     int               `json:"auto_fact_count,omitempty"`
	AutoQuestionCount int               `json:"auto_question_count,omitempty"`
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

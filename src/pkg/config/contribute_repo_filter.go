package config

import "strings"

// ContributeRepoFilter is the per-repository counterpart to the hive-wide
// contribute title/author/label filters. The list fields deliberately keep the
// same deny_* naming convention: the mode decides whether the list is an allow
// or deny list.
type ContributeRepoFilter struct {
	TitlesMode  string   `yaml:"titles_mode,omitempty" json:"titles_mode,omitempty"`
	AuthorsMode string   `yaml:"authors_mode,omitempty" json:"authors_mode,omitempty"`
	LabelsMode  string   `yaml:"labels_mode,omitempty" json:"labels_mode,omitempty"`
	DenyTitles  []string `yaml:"deny_titles,omitempty" json:"deny_titles,omitempty"`
	DenyAuthors []string `yaml:"deny_authors,omitempty" json:"deny_authors,omitempty"`
	DenyLabels  []string `yaml:"deny_labels,omitempty" json:"deny_labels,omitempty"`
	AllowLabels []string `yaml:"allow_labels,omitempty" json:"allow_labels,omitempty"`
}

// ContributeFilterDecision describes the first admission filter that rejected a
// candidate. A zero Reason means the candidate passed all configured filters.
type ContributeFilterDecision struct {
	Reason string
	Scope  string
	Repo   string
	Filter string
	Mode   string
	Match  string
}

func (d ContributeFilterDecision) Admitted() bool { return d.Reason == "" }

// NormalizeContributeRepoFilters normalizes modes and migrates legacy
// allow_labels entries inside per-repo filters the same way hub-wide config does.
func (h *HubConfig) NormalizeContributeRepoFilters() {
	if h == nil || len(h.ContributeRepoFilters) == 0 {
		return
	}
	for repo, filter := range h.ContributeRepoFilters {
		filter.TitlesMode = NormalizeFilterMode(filter.TitlesMode)
		filter.AuthorsMode = NormalizeFilterMode(filter.AuthorsMode)
		filter.LabelsMode = NormalizeFilterMode(filter.LabelsMode)
		if len(filter.AllowLabels) > 0 && len(filter.DenyLabels) == 0 && filter.LabelsMode == FilterModeDeny {
			filter.DenyLabels = filter.AllowLabels
			filter.LabelsMode = FilterModeAllow
			filter.AllowLabels = nil
		}
		h.ContributeRepoFilters[repo] = filter
	}
}

// ContributeRepoFilterFor returns the filter configured for a full owner/name
// repository key. Matching is case-insensitive but intentionally does not map a
// bare repo name to the current org: per-repo filters are keyed by full names.
func (h HubConfig) ContributeRepoFilterFor(repo string) (ContributeRepoFilter, bool) {
	key := strings.ToLower(strings.TrimSpace(repo))
	if key == "" || len(h.ContributeRepoFilters) == 0 {
		return ContributeRepoFilter{}, false
	}
	if f, ok := h.ContributeRepoFilters[repo]; ok {
		return normalizeContributeRepoFilter(f), true
	}
	for configured, f := range h.ContributeRepoFilters {
		if strings.ToLower(strings.TrimSpace(configured)) == key {
			return normalizeContributeRepoFilter(f), true
		}
	}
	return ContributeRepoFilter{}, false
}

// EvaluateContributeFilters applies the hive-wide skip labels and admission
// filters first, then the matching repository's filter. A repo filter can only
// narrow admission: deny mode extends the hive deny lists; allow mode requires a
// match for that repo only.
func (h HubConfig) EvaluateContributeFilters(repo, title, author string, labels []string) ContributeFilterDecision {
	if label, ok := h.MatchContributeSkipLabel(labels); ok {
		return ContributeFilterDecision{Reason: "skip_label", Scope: "hive", Filter: "label", Mode: FilterModeDeny, Match: label}
	}
	if d := evaluateContributeFilterScope("hive", "", ContributeRepoFilter{
		TitlesMode:  h.ContributeTitlesMode,
		AuthorsMode: h.ContributeAuthorsMode,
		LabelsMode:  h.ContributeLabelsMode,
		DenyTitles:  h.ContributeDenyTitles,
		DenyAuthors: h.ContributeDenyAuthors,
		DenyLabels:  h.ContributeDenyLabels,
	}, title, author, labels); !d.Admitted() {
		return d
	}
	if f, ok := h.ContributeRepoFilterFor(repo); ok {
		if d := evaluateContributeFilterScope("repo", repo, f, title, author, labels); !d.Admitted() {
			return d
		}
	}
	return ContributeFilterDecision{}
}

func normalizeContributeRepoFilter(f ContributeRepoFilter) ContributeRepoFilter {
	f.TitlesMode = NormalizeFilterMode(f.TitlesMode)
	f.AuthorsMode = NormalizeFilterMode(f.AuthorsMode)
	f.LabelsMode = NormalizeFilterMode(f.LabelsMode)
	if len(f.AllowLabels) > 0 && len(f.DenyLabels) == 0 && f.LabelsMode == FilterModeDeny {
		f.DenyLabels = f.AllowLabels
		f.LabelsMode = FilterModeAllow
		f.AllowLabels = nil
	}
	return f
}

func evaluateContributeFilterScope(scope, repo string, f ContributeRepoFilter, title, author string, labels []string) ContributeFilterDecision {
	if match, ok := valueFilterRejects(title, f.DenyTitles, f.TitlesMode); ok {
		return ContributeFilterDecision{Reason: "filter", Scope: scope, Repo: repo, Filter: "title", Mode: NormalizeFilterMode(f.TitlesMode), Match: match}
	}
	if match, ok := valueFilterRejects(author, f.DenyAuthors, f.AuthorsMode); ok {
		return ContributeFilterDecision{Reason: "filter", Scope: scope, Repo: repo, Filter: "author", Mode: NormalizeFilterMode(f.AuthorsMode), Match: match}
	}
	if match, ok := labelsFilterRejects(labels, f.DenyLabels, f.LabelsMode); ok {
		return ContributeFilterDecision{Reason: "filter", Scope: scope, Repo: repo, Filter: "label", Mode: NormalizeFilterMode(f.LabelsMode), Match: match}
	}
	return ContributeFilterDecision{}
}

func valueFilterRejects(value string, list []string, mode string) (string, bool) {
	switch NormalizeFilterMode(mode) {
	case FilterModeAllow:
		if len(list) == 0 || MatchesAny(value, list) {
			return "", false
		}
		return value, true
	default:
		for _, pattern := range list {
			if WildcardMatch(value, pattern) {
				return pattern, true
			}
		}
		return "", false
	}
}

func labelsFilterRejects(labels []string, list []string, mode string) (string, bool) {
	switch NormalizeFilterMode(mode) {
	case FilterModeAllow:
		if len(list) == 0 {
			return "", false
		}
		for _, label := range labels {
			if MatchesAny(label, list) {
				return "", false
			}
		}
		return strings.Join(list, ","), true
	default:
		for _, label := range labels {
			for _, pattern := range list {
				if WildcardMatch(label, pattern) {
					return label, true
				}
			}
		}
		return "", false
	}
}

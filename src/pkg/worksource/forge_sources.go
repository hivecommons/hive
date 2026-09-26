package worksource

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/forge"
)

type forgeRepoConfig struct {
	SourceRepo string
	WorkRepo   string
}

type forgeWorkSourceConfig struct {
	Kind       forge.Kind
	BaseURL    string
	Token      string
	TokenEnv   string
	Org        string
	Repos      []forgeRepoConfig
	States     []string
	Labels     []string
	Assignee   string
	HoldLabels []string
}

type forgeWorkSource struct {
	cfg    forgeWorkSourceConfig
	client forge.Forge
}

type forgeIssueStateSetter interface {
	SetIssueState(ctx context.Context, repo string, number int, state string) error
}

func newGiteaSource(cfg forgeWorkSourceConfig) (WorkSource, error) {
	cfg.Kind = forge.KindGitea
	if cfg.TokenEnv == "" {
		cfg.TokenEnv = config.DefaultGiteaTokenEnv
	}
	return newForgeWorkSource(cfg)
}

func newGitLabSource(cfg forgeWorkSourceConfig) (WorkSource, error) {
	cfg.Kind = forge.KindGitLab
	if cfg.TokenEnv == "" {
		cfg.TokenEnv = config.DefaultGitLabTokenEnv
	}
	return newForgeWorkSource(cfg)
}

func newForgeWorkSource(cfg forgeWorkSourceConfig) (*forgeWorkSource, error) {
	if strings.TrimSpace(cfg.BaseURL) == "" && cfg.Kind == forge.KindGitea {
		return nil, fmt.Errorf("work_source.%s.base_url is required", cfg.Kind)
	}
	if len(cfg.Repos) == 0 {
		return nil, fmt.Errorf("work_source.%s.repos must contain at least one repository", cfg.Kind)
	}
	for i := range cfg.Repos {
		cfg.Repos[i].SourceRepo = strings.TrimSpace(cfg.Repos[i].SourceRepo)
		cfg.Repos[i].WorkRepo = strings.TrimSpace(cfg.Repos[i].WorkRepo)
		if cfg.Repos[i].SourceRepo == "" {
			return nil, fmt.Errorf("work_source.%s.repos[%d].repo is required", cfg.Kind, i)
		}
		if cfg.Repos[i].WorkRepo == "" {
			cfg.Repos[i].WorkRepo = cfg.Repos[i].SourceRepo
		}
	}
	token := strings.TrimSpace(cfg.Token)
	var err error
	if token != "" {
		token, err = resolveSecretRef(fmt.Sprintf("work_source.%s.token", cfg.Kind), token)
		if err != nil {
			return nil, err
		}
	}
	if token == "" && cfg.TokenEnv != "" {
		token = os.Getenv(cfg.TokenEnv)
	}
	client, err := forge.NewForge(cfg.Kind, token, forge.Options{BaseURL: cfg.BaseURL, Org: cfg.Org})
	if err != nil {
		return nil, fmt.Errorf("work_source.%s: %w", cfg.Kind, err)
	}
	cfg.Token = token
	return &forgeWorkSource{cfg: cfg, client: client}, nil
}

func (s *forgeWorkSource) SourceType() string { return string(s.cfg.Kind) }

func (s *forgeWorkSource) ListIssues(ctx context.Context) ([]Issue, error) {
	if s == nil || s.client == nil {
		return nil, fmt.Errorf("worksource/%s: client unavailable", s.cfg.Kind)
	}
	out := make([]Issue, 0)
	for _, repo := range s.cfg.Repos {
		items, err := s.client.ListOpenIssues(ctx, repo.SourceRepo)
		if err != nil {
			return nil, fmt.Errorf("worksource/%s: list %s: %w", s.cfg.Kind, repo.SourceRepo, err)
		}
		for _, it := range items {
			if !s.includeIssue(it) {
				continue
			}
			issue := Issue{
				SourceType: string(s.cfg.Kind),
				Repo:       repo.WorkRepo,
				ExternalID: forgeExternalID(repo.SourceRepo, it.Number),
				Number:     0,
				Title:      it.Title,
				Author:     it.Author,
				Labels:     it.Labels,
				Assignees:  it.Assignees,
				State:      it.State,
				CreatedAt:  it.CreatedAt,
				URL:        it.URL,
			}
			if RefFromIssue(issue).Key() != "" {
				out = append(out, issue)
			}
		}
	}
	return out, nil
}

func (s *forgeWorkSource) AddLabel(ctx context.Context, ref Ref, label string) error {
	repo, number, err := s.nativeRef(ref)
	if err != nil {
		return err
	}
	return s.client.AddLabels(ctx, repo, number, []string{strings.TrimSpace(label)})
}

func (s *forgeWorkSource) RemoveLabel(ctx context.Context, ref Ref, label string) error {
	repo, number, err := s.nativeRef(ref)
	if err != nil {
		return err
	}
	return s.client.RemoveLabel(ctx, repo, number, strings.TrimSpace(label))
}

func (s *forgeWorkSource) AddComment(ctx context.Context, ref Ref, body string) error {
	repo, number, err := s.nativeRef(ref)
	if err != nil {
		return err
	}
	return s.client.CreateIssueComment(ctx, repo, number, body)
}

func (s *forgeWorkSource) TransitionStatus(ctx context.Context, ref Ref, status string) error {
	setter, ok := s.client.(forgeIssueStateSetter)
	if !ok {
		return ErrStatusTransitionUnsupported
	}
	repo, number, err := s.nativeRef(ref)
	if err != nil {
		return err
	}
	state, ok := s.nativeTransition(status)
	if !ok {
		return ErrStatusTransitionUnsupported
	}
	return setter.SetIssueState(ctx, repo, number, state)
}

func (s *forgeWorkSource) nativeTransition(status string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "close", "closed", "done", "resolved":
		if s.cfg.Kind == forge.KindGitLab {
			return "close", true
		}
		return "closed", true
	case "open", "opened", "reopen", "reopened":
		if s.cfg.Kind == forge.KindGitLab {
			return "reopen", true
		}
		return "open", true
	default:
		return "", false
	}
}

func (s *forgeWorkSource) nativeRef(ref Ref) (string, int, error) {
	if s == nil || s.client == nil {
		return "", 0, fmt.Errorf("worksource/%s: client unavailable", s.cfg.Kind)
	}
	id := strings.TrimSpace(ref.ExternalID)
	if id == "" && ref.Number > 0 {
		id = strconv.Itoa(ref.Number)
	}
	sourceRepo, id := splitForgeExternalID(id)
	number, err := strconv.Atoi(id)
	if err != nil || number <= 0 {
		return "", 0, fmt.Errorf("worksource/%s: numeric external id is required", s.cfg.Kind)
	}
	repo := strings.TrimSpace(sourceRepo)
	if repo == "" {
		repo = s.sourceRepoForWorkRepo(strings.TrimSpace(ref.Repo))
	}
	if repo == "" {
		return "", 0, fmt.Errorf("worksource/%s: repo is required", s.cfg.Kind)
	}
	return repo, number, nil
}

func forgeExternalID(sourceRepo string, number int) string {
	if strings.TrimSpace(sourceRepo) == "" {
		return strconv.Itoa(number)
	}
	return strings.TrimSpace(sourceRepo) + "#" + strconv.Itoa(number)
}

func splitForgeExternalID(id string) (sourceRepo, number string) {
	if repo, num, ok := strings.Cut(strings.TrimSpace(id), "#"); ok && strings.TrimSpace(repo) != "" {
		return strings.TrimSpace(repo), strings.TrimSpace(num)
	}
	return "", strings.TrimSpace(id)
}

func (s *forgeWorkSource) sourceRepoForWorkRepo(workRepo string) string {
	for _, repo := range s.cfg.Repos {
		if workRepo == repo.WorkRepo || workRepo == repo.SourceRepo {
			return repo.SourceRepo
		}
	}
	return workRepo
}

func (s *forgeWorkSource) includeIssue(it forge.Issue) bool {
	if len(s.cfg.States) > 0 && !containsFold(s.cfg.States, it.State) {
		return false
	}
	if strings.TrimSpace(s.cfg.Assignee) != "" && !containsFold(it.Assignees, s.cfg.Assignee) {
		return false
	}
	if hasAnyFold(it.Labels, append(defaultLinearHoldLabels, s.cfg.HoldLabels...)) {
		return false
	}
	for _, want := range s.cfg.Labels {
		if strings.TrimSpace(want) != "" && !containsFold(it.Labels, want) {
			return false
		}
	}
	return true
}

func containsFold(values []string, want string) bool {
	want = strings.TrimSpace(want)
	for _, v := range values {
		if strings.EqualFold(strings.TrimSpace(v), want) {
			return true
		}
	}
	return false
}

func hasAnyFold(values, wants []string) bool {
	for _, want := range wants {
		if strings.TrimSpace(want) != "" && containsFold(values, want) {
			return true
		}
	}
	return false
}

func forgeReposFromConfig(in []config.ForgeWorkRepoSourceConfig) []forgeRepoConfig {
	out := make([]forgeRepoConfig, 0, len(in))
	for _, r := range in {
		out = append(out, forgeRepoConfig{SourceRepo: r.Repo, WorkRepo: r.WorkRepo})
	}
	return out
}

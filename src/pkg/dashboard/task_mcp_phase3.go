package dashboard

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/beads"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/knowledge"
	"github.com/hivecommons/hive/pkg/planning"
	"github.com/hivecommons/hive/pkg/taskmcp"
)

func (p dashboardTaskMCPProvider) RepoConventions(ctx context.Context, scope taskmcp.Scope, page taskmcp.PageRequest) (taskmcp.RepoConventionsData, taskmcp.PageInfo, error) {
	snap, err := p.snapshotForScope(scope)
	if err != nil {
		return taskmcp.RepoConventionsData{}, taskmcp.PageInfo{}, err
	}
	repo := snap.assign.Repo
	items, source := p.repoConventionItems(ctx, repo)
	items, info := taskmcp.PaginateConventions(limitConventions(items), page)
	return taskmcp.RepoConventionsData{Repo: repo, Source: source, Conventions: items}, info, nil
}

func (p dashboardTaskMCPProvider) Dependencies(_ context.Context, scope taskmcp.Scope, page taskmcp.PageRequest) (taskmcp.DependenciesData, taskmcp.PageInfo, error) {
	snap, err := p.snapshotForScope(scope)
	if err != nil {
		return taskmcp.DependenciesData{}, taskmcp.PageInfo{}, err
	}
	data := p.dependencyData(snap)
	data, info := taskmcp.PaginateDependencies(limitDependencies(data, p.dependenciesLimit()), page)
	return data, info, nil
}

func (p dashboardTaskMCPProvider) History(_ context.Context, scope taskmcp.Scope, page taskmcp.PageRequest) (taskmcp.HistoryData, taskmcp.PageInfo, error) {
	scope, current, candidates := p.relatedWorkSnapshot(scope)
	window := time.Duration(config.DefaultTaskMCPRelatedWorkRecencyDays) * 24 * time.Hour
	limit := config.DefaultTaskMCPHistoryLimit
	if cfg := p.config(); cfg != nil {
		window = time.Duration(cfg.Hub.TaskMCPRelatedWorkRecencyDaysOrDefault()) * 24 * time.Hour
		limit = cfg.Hub.TaskMCPHistoryLimitOrDefault()
	}
	items := taskmcp.FilterHistory(scope, current, candidates, window, time.Now(), limit)
	data, info := taskmcp.PaginateHistory(items, page)
	return data, info, nil
}

func (p dashboardTaskMCPProvider) Knowledge(ctx context.Context, scope taskmcp.Scope, query string, page taskmcp.PageRequest) (taskmcp.KnowledgeData, taskmcp.PageInfo, error) {
	snap, err := p.snapshotForScope(scope)
	if err != nil {
		return taskmcp.KnowledgeData{}, taskmcp.PageInfo{}, err
	}
	items := p.knowledgeItems(ctx, snap.assign.Repo, query, p.knowledgeLimit())
	data, info := taskmcp.PaginateKnowledge(items, page)
	return data, info, nil
}

func (p dashboardTaskMCPProvider) repoConventionItems(ctx context.Context, repo string) ([]taskmcp.RepoConvention, string) {
	facts := p.scopedKnowledgeFacts(ctx, repo, "conventions", p.knowledgeLimit())
	var human, derived []taskmcp.RepoConvention
	for _, fact := range facts {
		if !isRepoConventionFact(fact, repo) {
			continue
		}
		if p.server != nil && p.server.deps != nil && p.server.deps.Knowledge != nil {
			fact = p.server.deps.Knowledge.FullFact(ctx, fact)
		}
		item := taskmcp.RepoConvention{
			Slug:   fact.Slug,
			Layer:  string(fact.Layer),
			Source: conventionSource(fact),
			Data:   taskmcp.ServedText{Title: fact.Title, Body: fact.Body},
		}
		if item.Source == "derived" {
			derived = append(derived, item)
		} else {
			item.Source = "human"
			human = append(human, item)
		}
	}
	if len(human) > 0 {
		return human, "human"
	}
	if len(derived) > 0 {
		return derived, "derived"
	}
	return []taskmcp.RepoConvention{}, "none"
}

func (p dashboardTaskMCPProvider) knowledgeItems(ctx context.Context, repo, query string, limit int) []taskmcp.KnowledgeItem {
	facts := p.scopedKnowledgeFacts(ctx, repo, query, limit)
	items := make([]taskmcp.KnowledgeItem, 0, len(facts))
	for _, fact := range facts {
		if !factForRepo(fact, repo) {
			continue
		}
		items = append(items, taskmcp.KnowledgeItem{
			Repo:       repo,
			Slug:       fact.Slug,
			Layer:      string(fact.Layer),
			Confidence: fact.Confidence,
			Tags:       append([]string(nil), fact.Tags...),
			Data:       taskmcp.ServedText{Title: fact.Title, Body: fact.Body},
		})
	}
	if len(items) > limit {
		items = items[:limit]
	}
	return items
}

func (p dashboardTaskMCPProvider) scopedKnowledgeFacts(ctx context.Context, repo, query string, limit int) []knowledge.Fact {
	if p.server == nil || p.server.deps == nil || p.server.deps.Knowledge == nil {
		return nil
	}
	query = strings.TrimSpace(query)
	facts := p.server.deps.Knowledge.SearchAllWithVaults(ctx, "", "", 0)
	if query != "" {
		searchLimit := limit * 5
		if searchLimit < 100 {
			searchLimit = 100
		}
		facts = append(facts, p.server.deps.Knowledge.SearchAllWithVaults(ctx, query, "", searchLimit)...)
	}
	out := make([]knowledge.Fact, 0, len(facts))
	seen := map[string]bool{}
	for _, fact := range facts {
		key := string(fact.Layer) + "/" + fact.Slug
		if !factForRepo(fact, repo) || !factMatchesQuery(fact, query) {
			continue
		}
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, fact)
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out
}

func (p dashboardTaskMCPProvider) dependencyData(snap taskMCPSnapshot) taskmcp.DependenciesData {
	data := taskmcp.DependenciesData{TaskID: snap.assign.TaskID, BlockedBy: []taskmcp.DependencyNode{}, Blocks: []taskmcp.DependencyNode{}}
	if p.server == nil || p.server.deps == nil {
		return data
	}
	currentRefs := []string{snap.assign.TaskID, snap.assign.Key, fmt.Sprintf("%s#%d", snap.assign.Repo, snap.assign.Number), "gh-" + fmt.Sprintf("%s#%d", snap.assign.Repo, snap.assign.Number)}
	nodeByID := map[string]taskmcp.DependencyNode{}
	var currentIDs []string
	for _, store := range p.server.deps.BeadStores {
		if store == nil {
			continue
		}
		store.ReadEach(beads.ListFilter{}, func(b *beads.Bead) {
			nodeByID[b.ID] = taskmcp.DependencyNode{ID: b.ID, Title: b.Title}
			if beadMatchesAnyRef(b, currentRefs) {
				currentIDs = append(currentIDs, b.ID)
			}
		})
	}
	currentSet := map[string]bool{}
	for _, id := range currentIDs {
		currentSet[id] = true
	}
	for _, store := range p.server.deps.BeadStores {
		if store == nil {
			continue
		}
		store.ReadEach(beads.ListFilter{}, func(b *beads.Bead) {
			if currentSet[b.ID] {
				for _, depID := range b.DependsOn {
					if node, ok := nodeByID[depID]; ok {
						data.BlockedBy = appendUniqueNode(data.BlockedBy, node)
					}
				}
				if b.Meta(planning.MetaPlanStatus) != "" {
					data.SuggestedPlan = append(data.SuggestedPlan, planChildrenFor(p.server.deps.BeadStores, b.ID)...)
				}
			}
			for _, depID := range b.DependsOn {
				if currentSet[depID] {
					data.Blocks = appendUniqueNode(data.Blocks, taskmcp.DependencyNode{ID: b.ID, Title: b.Title})
				}
			}
			if currentSet[b.Meta(planning.MetaParentEpic)] {
				data.SuggestedPlan = appendUniqueNode(data.SuggestedPlan, taskmcp.DependencyNode{ID: b.ID, Title: b.Title})
			}
		})
	}
	sortNodes(data.BlockedBy)
	sortNodes(data.Blocks)
	sortNodes(data.SuggestedPlan)
	return data
}

func planChildrenFor(stores map[string]*beads.Store, epicID string) []taskmcp.DependencyNode {
	var out []taskmcp.DependencyNode
	for _, store := range stores {
		if store == nil {
			continue
		}
		store.ReadEach(beads.ListFilter{}, func(b *beads.Bead) {
			if b.Meta(planning.MetaParentEpic) == epicID {
				out = appendUniqueNode(out, taskmcp.DependencyNode{ID: b.ID, Title: b.Title})
			}
		})
	}
	return out
}

func beadMatchesAnyRef(b *beads.Bead, refs []string) bool {
	for _, ref := range refs {
		if ref != "" && (b.ID == ref || b.ExternalRef == ref) {
			return true
		}
	}
	return false
}

func appendUniqueNode(nodes []taskmcp.DependencyNode, node taskmcp.DependencyNode) []taskmcp.DependencyNode {
	if node.ID == "" {
		return nodes
	}
	for _, existing := range nodes {
		if existing.ID == node.ID {
			return nodes
		}
	}
	return append(nodes, node)
}

func sortNodes(nodes []taskmcp.DependencyNode) {
	sort.SliceStable(nodes, func(i, j int) bool { return nodes[i].ID < nodes[j].ID })
}

func limitDependencies(data taskmcp.DependenciesData, limit int) taskmcp.DependenciesData {
	if limit <= 0 {
		limit = taskmcp.MaxTaskMCPDependencies
	}
	take := func(in []taskmcp.DependencyNode, remaining *int) []taskmcp.DependencyNode {
		if *remaining <= 0 {
			return []taskmcp.DependencyNode{}
		}
		if len(in) > *remaining {
			in = in[:*remaining]
		}
		*remaining -= len(in)
		if in == nil {
			return []taskmcp.DependencyNode{}
		}
		return in
	}
	remaining := limit
	data.BlockedBy = take(data.BlockedBy, &remaining)
	data.Blocks = take(data.Blocks, &remaining)
	data.SuggestedPlan = take(data.SuggestedPlan, &remaining)
	return data
}

func limitConventions(items []taskmcp.RepoConvention) []taskmcp.RepoConvention {
	if len(items) > taskmcp.MaxTaskMCPRepoConventions {
		return items[:taskmcp.MaxTaskMCPRepoConventions]
	}
	return items
}

func (p dashboardTaskMCPProvider) knowledgeLimit() int {
	if cfg := p.config(); cfg != nil {
		return cfg.Hub.TaskMCPKnowledgeLimitOrDefault()
	}
	return config.DefaultTaskMCPKnowledgeLimit
}

func (p dashboardTaskMCPProvider) dependenciesLimit() int {
	if cfg := p.config(); cfg != nil {
		return cfg.Hub.TaskMCPDependenciesLimitOrDefault()
	}
	return config.DefaultTaskMCPDependenciesLimit
}

func factForRepo(f knowledge.Fact, repo string) bool {
	want := "repo:" + strings.ToLower(strings.TrimSpace(repo))
	for _, tag := range f.Tags {
		if strings.EqualFold(strings.TrimSpace(tag), want) {
			return true
		}
	}
	return false
}

func factMatchesQuery(f knowledge.Fact, query string) bool {
	query = strings.ToLower(strings.TrimSpace(query))
	if query == "" {
		return true
	}
	haystack := strings.ToLower(f.Title + "\n" + f.Body + "\n" + strings.Join(f.Tags, "\n"))
	return strings.Contains(haystack, query)
}

func isRepoConventionFact(f knowledge.Fact, repo string) bool {
	if !factForRepo(f, repo) {
		return false
	}
	if f.Type == knowledge.FactConventions {
		return true
	}
	for _, tag := range f.Tags {
		if strings.EqualFold(tag, "kind:conventions") || strings.EqualFold(tag, string(knowledge.FactConventions)) {
			return true
		}
	}
	return false
}

func conventionSource(f knowledge.Fact) string {
	for _, tag := range f.Tags {
		if strings.EqualFold(tag, "source:derived") {
			return "derived"
		}
	}
	return "human"
}

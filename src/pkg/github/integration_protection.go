package github

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
)

const integrationProtectionMaxRules = 100

// ReadIntegrationProtectionDigest observes the resolved active rules and
// classic protection. It never interprets a permission/read failure as absent
// protection, and rejects a native-queue requirement rather than falling back.
func (c *Client) ReadIntegrationProtectionDigest(ctx context.Context, repo, base string) (string, error) {
	return c.ReadIntegrationProtectionDigestForActor(ctx, repo, base, 0)
}

func (c *Client) ReadIntegrationProtectionDigestForActor(ctx context.Context, repo, base string, appID int64) (string, error) {
	if c == nil || c.client == nil {
		return "", ErrNoGitHubClient
	}
	if !integrationPromotionRepo(repo) || !integrationPromotionBranch(base) {
		return "", fmt.Errorf("invalid exact protected destination")
	}
	path := "repos/" + repo + "/branches/" + url.PathEscape(base)
	var branch struct {
		Protected bool `json:"protected"`
	}
	if err := c.integrationCandidateRead(ctx, path, &branch); err != nil {
		return "", err
	}
	if !branch.Protected {
		return "", fmt.Errorf("Hive exact-object promotion requires an actually protected destination")
	}
	var rules []map[string]any
	if err := c.integrationCandidateRead(ctx, "repos/"+repo+"/rules/branches/"+url.PathEscape(base)+"?per_page=100", &rules); err != nil {
		return "", fmt.Errorf("resolved branch rules unknown: %w", err)
	}
	if len(rules) >= integrationProtectionMaxRules {
		return "", fmt.Errorf("resolved protection cohort possibly truncated")
	}
	for _, rule := range rules {
		if rule["type"] == "merge_queue" {
			return "", fmt.Errorf("GitHub native queue owns this destination; Hive promotion forbidden")
		}
	}
	rulesets := map[string]any{}
	for _, rule := range rules {
		id, valid := rule["ruleset_id"].(float64)
		source, sourceKnown := rule["ruleset_source"].(string)
		if !valid || !sourceKnown || id <= 0 || id != float64(int64(id)) {
			return "", fmt.Errorf("resolved ruleset identity unknown")
		}
		path := ""
		switch rule["ruleset_source_type"] {
		case "Repository":
			if source != repo {
				return "", fmt.Errorf("resolved repository ruleset identity changed")
			}
			path = "repos/" + repo + "/rulesets/" + strconv.FormatInt(int64(id), 10)
		case "Organization":
			if source != strings.SplitN(repo, "/", 2)[0] {
				return "", fmt.Errorf("resolved organization ruleset identity changed")
			}
			path = "orgs/" + source + "/rulesets/" + strconv.FormatInt(int64(id), 10)
		default:
			return "", fmt.Errorf("ruleset authority unsupported/unknown")
		}
		if _, exists := rulesets[path]; exists {
			continue
		}
		if len(rulesets) >= integrationReconcileProtectionRuleLimit {
			return "", fmt.Errorf("resolved ruleset metadata budget exhausted")
		}
		var metadata map[string]any
		if err := c.integrationCandidateRead(ctx, path, &metadata); err != nil {
			return "", fmt.Errorf("ruleset actor/enforcement metadata unknown: %w", err)
		}
		actors, complete := metadata["bypass_actors"].([]any)
		if !complete || metadata["enforcement"] != "active" {
			return "", fmt.Errorf("active ruleset bypass cohort incomplete")
		}
		// v1 qualification deliberately supports only a non-bypassing actor
		// policy. A successful privileged push is not proof checks were required.
		if appID > 0 && len(actors) > 0 {
			return "", fmt.Errorf("v1 exact-object qualification requires no ruleset bypass actors")
		}
		rulesets[path] = integrationProtectionValue(metadata)
	}
	var classic map[string]any
	if err := c.integrationCandidateRead(ctx, path+"/protection", &classic); err != nil {
		// 404 can hide missing permission, not only absent classic protection.
		return "", fmt.Errorf("classic protection unknown (including ambiguous absence): %w", err)
	}
	if appID > 0 {
		reviews, _ := classic["required_pull_request_reviews"].(map[string]any)
		bypass, _ := reviews["bypass_pull_request_allowances"].(map[string]any)
		for _, kind := range []string{"apps", "users", "teams"} {
			actors, _ := bypass[kind].([]any)
			if len(actors) > 0 {
				return "", fmt.Errorf("v1 qualification refuses classic review bypass allowances")
			}
		}
	}
	if len(rules) == 0 && len(classic) == 0 {
		return "", fmt.Errorf("protected destination policy unavailable")
	}
	canonicalRules := make([]string, 0, len(rules))
	for _, rule := range rules {
		raw, err := json.Marshal(integrationProtectionValue(rule))
		if err != nil {
			return "", err
		}
		canonicalRules = append(canonicalRules, string(raw))
	}
	sort.Strings(canonicalRules)
	raw, err := json.Marshal(map[string]any{"rules": canonicalRules, "rulesets": rulesets, "classic": integrationProtectionValue(classic)})
	if err != nil {
		return "", err
	}
	hash := sha256.Sum256(raw)
	return hex.EncodeToString(hash[:]), nil
}

const integrationReconcileProtectionRuleLimit = 8

func integrationProtectionValue(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(typed))
		for key, item := range typed {
			// Branch-specific hyperlinks are not policy. All enforcement,
			// actor, workflow, check and ruleset identity fields are retained.
			if key == "url" || strings.HasSuffix(key, "_url") {
				continue
			}
			out[key] = integrationProtectionValue(item)
		}
		return out
	case []any:
		out := make([]any, len(typed))
		for i, item := range typed {
			out[i] = integrationProtectionValue(item)
		}
		return out
	default:
		return typed
	}
}

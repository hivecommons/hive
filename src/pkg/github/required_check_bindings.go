package github

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"

	gh "github.com/google/go-github/v72/github"
)

func requiredBindings(contexts *[]string, checks *[]*gh.RequiredStatusCheck) []PRRequiredCheck {
	byName := map[string][]PRRequiredCheck{}
	if contexts != nil {
		for _, name := range *contexts {
			if strings.TrimSpace(name) != "" {
				byName[name] = []PRRequiredCheck{{Name: name}}
			}
		}
	}
	// contexts is the compatibility projection of checks, not an additional
	// any-App rule that can override a pinned producer.
	seen := map[string]bool{}
	if checks != nil {
		for _, check := range *checks {
			if check == nil || strings.TrimSpace(check.Context) == "" {
				continue
			}
			if !seen[check.Context] {
				byName[check.Context] = nil
				seen[check.Context] = true
			}
			byName[check.Context] = append(byName[check.Context], PRRequiredCheck{Name: check.Context, AppID: clonePtr(check.AppID), AppKnown: true})
		}
	}
	var out []PRRequiredCheck
	for _, checks := range byName {
		out = append(out, checks...)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		if out[i].AppID == nil || out[j].AppID == nil {
			return out[i].AppID == nil && out[j].AppID != nil
		}
		return *out[i].AppID < *out[j].AppID
	})
	return out
}

type requiredCheckWire struct {
	Context string          `json:"context"`
	AppID   json.RawMessage `json:"app_id"`
}

func bindingsFromWire(contexts []string, checks []requiredCheckWire) []PRRequiredCheck {
	var typed []*gh.RequiredStatusCheck
	for _, check := range checks {
		var id *int64
		_ = json.Unmarshal(check.AppID, &id)
		typed = append(typed, &gh.RequiredStatusCheck{Context: check.Context, AppID: id})
	}
	out := requiredBindings(&contexts, &typed)
	for i := range out {
		for _, check := range checks {
			if check.Context == out[i].Name {
				var id *int64
				if len(check.AppID) == 0 || json.Unmarshal(check.AppID, &id) != nil {
					out[i].AppKnown = false
				}
			}
		}
	}
	return out
}

func observedRequiredBindings(names map[string]bool, bindings []PRRequiredCheck) bool {
	for name := range names {
		found := false
		for _, check := range bindings {
			if check.Name == name {
				if !check.AppKnown || (check.AppID != nil && *check.AppID != -1 && *check.AppID <= 0) {
					return false
				}
				found = true
			}
		}
		if !found {
			return false
		}
	}
	return true
}

func requiredStatusChecksFromProtectionEndpoint(ctx context.Context, client *gh.Client, owner, repo, branch string) (map[string]bool, []PRRequiredCheck, error) {
	request, err := client.NewRequest(http.MethodGet, fmt.Sprintf("repos/%s/%s/branches/%s/protection/required_status_checks", owner, repo, url.PathEscape(branch)), nil)
	if err != nil {
		return nil, nil, err
	}
	var response *struct {
		Contexts []string            `json:"contexts"`
		Checks   []requiredCheckWire `json:"checks"`
	}
	reply, err := client.Do(ctx, request, &response)
	if reply != nil && reply.StatusCode == http.StatusNotFound {
		return nil, nil, gh.ErrBranchNotProtected
	}
	if err != nil {
		return nil, nil, err
	}
	names := map[string]bool{}
	if response == nil {
		return names, nil, nil
	}
	names = requiredContextsFromRequiredStatusChecks(&response.Contexts, nil)
	bindings := bindingsFromWire(response.Contexts, response.Checks)
	for _, check := range bindings {
		names[check.Name] = true
	}
	return names, bindings, nil
}

func requiredBindingsFromRules(payload any) []PRRequiredCheck {
	var checks []*gh.RequiredStatusCheck
	known := map[string]bool{}
	var collectChecks func(any)
	collectChecks = func(v any) {
		switch x := v.(type) {
		case []any:
			for _, item := range x {
				collectChecks(item)
			}
		case map[string]any:
			collectChecks(x["required_status_checks"])
			collectChecks(x["required_status_check_contexts"])
			if name, ok := x["context"].(string); ok && strings.TrimSpace(name) != "" {
				var appID *int64
				rawID, present := x["integration_id"]
				valid := present && rawID == nil
				if id, ok := x["integration_id"].(float64); ok {
					const maxExactRequiredAppID = 1 << 53
					if id <= maxExactRequiredAppID && id >= -1 && id != 0 && id == float64(int64(id)) {
						value := int64(id)
						appID, valid = &value, true
					}
				}
				if previous, exists := known[name]; exists {
					known[name] = previous && valid
				} else {
					known[name] = valid
				}
				checks = append(checks, &gh.RequiredStatusCheck{Context: name, AppID: appID})
			}
		case string:
			if strings.TrimSpace(x) != "" {
				checks = append(checks, &gh.RequiredStatusCheck{Context: x})
				known[x] = false
			}
		}
	}
	var walk func(any)
	walk = func(v any) {
		switch x := v.(type) {
		case []any:
			for _, item := range x {
				walk(item)
			}
		case map[string]any:
			if typ, _ := x["type"].(string); typ == "required_status_checks" {
				collectChecks(x["parameters"])
				collectChecks(x["required_status_checks"])
			} else {
				walk(x["rules"])
			}
		}
	}
	walk(payload)
	out := requiredBindings(nil, &checks)
	for i := range out {
		out[i].AppKnown = known[out[i].Name]
	}
	return out
}

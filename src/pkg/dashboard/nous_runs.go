package dashboard

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/worksource"
)

const nousOutputModeSpektacularRun = "spektacular-run"

type nousRunTarget struct {
	repo   string
	number int
	title  string
}

func (s *Server) approveNousSpektacularRun() (map[string]string, bool, error) {
	if s == nil || s.deps == nil || s.deps.Nous == nil {
		return nil, false, nil
	}
	ns := s.deps.Nous
	ns.Mu.Lock()
	defer ns.Mu.Unlock()
	if nousConfigOutputMode(ns.Config) != nousOutputModeSpektacularRun {
		return nil, false, nil
	}
	target, pending, err := nousPendingTarget(ns.Status)
	if err != nil {
		return nil, true, err
	}
	if err := s.AdmitRun(target.repo, target.number, target.title, time.Now()); err != nil {
		return nil, true, err
	}
	key := worksource.Ref{Repo: target.repo, Number: target.number}.Key()
	link := map[string]string{"key": key, "repo": target.repo, "number": strconv.Itoa(target.number), "stage": StageSpec}
	if ns.Status != nil {
		if pending != nil {
			pending = cloneNousMap(pending)
			pending["run"] = link
			pending["run_key"] = key
			ns.Status["pending"] = pending
		}
		ns.Status["run"] = link
	}
	return link, true, nil
}

func nousConfigOutputMode(cfg map[string]interface{}) string {
	output, ok := cfg["output"].(map[string]interface{})
	if !ok {
		return ""
	}
	mode, _ := output["mode"].(string)
	return strings.TrimSpace(mode)
}

func nousPendingTarget(status map[string]interface{}) (nousRunTarget, map[string]interface{}, error) {
	pending, ok := status["pending"].(map[string]interface{})
	if !ok || pending == nil {
		return nousRunTarget{}, nil, fmt.Errorf("pending proposal is required for %s output", nousOutputModeSpektacularRun)
	}
	repo, number := nousRepoNumber(pending)
	if repo == "" || number <= 0 {
		for _, key := range []string{"target", "run_target", "issue", "issue_url", "issueURL", "url"} {
			if repo != "" && number > 0 {
				break
			}
			if raw, ok := pending[key].(string); ok {
				if r, n, err := parseRunSpecTargetFromText(raw); err == nil {
					repo, number = r, n
				}
			}
		}
	}
	if repo == "" || number <= 0 {
		return nousRunTarget{}, pending, fmt.Errorf("%s output requires pending.repo and pending.number or an owner/repo#number target", nousOutputModeSpektacularRun)
	}
	title := firstString(pending, "title", "hypothesis", "summary", "id")
	if strings.TrimSpace(title) == "" {
		title = fmt.Sprintf("Strategy Lab campaign for %s#%d", repo, number)
	}
	return nousRunTarget{repo: repo, number: number, title: title}, pending, nil
}

func parseRunSpecTargetFromText(raw string) (string, int, error) {
	target := strings.TrimSpace(raw)
	if strings.HasPrefix(target, "https://github.com/") {
		parts := strings.Split(strings.TrimPrefix(target, "https://github.com/"), "/")
		if len(parts) >= 4 && parts[2] == "issues" {
			n, err := strconv.Atoi(parts[3])
			if err == nil && n > 0 {
				return parts[0] + "/" + parts[1], n, nil
			}
		}
	}
	return parseRunSpecTarget(target)
}

func nousRepoNumber(pending map[string]interface{}) (string, int) {
	repo := strings.TrimSpace(firstString(pending, "repo", "repository", "full_name"))
	number := nousIntFromAny(pending["number"])
	if number <= 0 {
		number = nousIntFromAny(pending["issue_number"])
	}
	return repo, number
}

func firstString(m map[string]interface{}, keys ...string) string {
	for _, key := range keys {
		if v, ok := m[key].(string); ok && strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func nousIntFromAny(v interface{}) int {
	switch n := v.(type) {
	case int:
		return n
	case int64:
		return int(n)
	case float64:
		return int(n)
	case json.Number:
		i, _ := strconv.Atoi(n.String())
		return i
	case string:
		i, _ := strconv.Atoi(strings.TrimSpace(n))
		return i
	default:
		return 0
	}
}

func cloneNousMap(in map[string]interface{}) map[string]interface{} {
	out := make(map[string]interface{}, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

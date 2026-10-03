package dashboard

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/worksource"
)

const nousOutputModeSpektacularRun = "spektacular-run"

type nousRunTarget struct {
	repo   string
	number int
	title  string
}

// nousRunApproval is the outcome of a Strategy Lab approve in
// spektacular-run output mode. handled is false when the mode is not active;
// existing marks a proposal whose run was already admitted, so a repeated
// approve reports that run instead of admitting it again (hivecommons/hive#10115).
type nousRunApproval struct {
	link     map[string]string
	handled  bool
	existing bool
}

// nousRunApprovalError carries the HTTP status an approve failure maps to:
// a malformed proposal is the client's fault, an unavailable registry or a
// failed admission is the server's.
type nousRunApprovalError struct {
	status int
	err    error
}

func (e *nousRunApprovalError) Error() string { return e.err.Error() }
func (e *nousRunApprovalError) Unwrap() error { return e.err }

func nousRunApprovalStatus(err error) int {
	var approvalErr *nousRunApprovalError
	if errors.As(err, &approvalErr) {
		return approvalErr.status
	}
	return http.StatusInternalServerError
}

func (s *Server) approveNousSpektacularRun() (nousRunApproval, error) {
	if s == nil || s.deps == nil || s.deps.Nous == nil {
		return nousRunApproval{}, nil
	}
	ns := s.deps.Nous
	ns.Mu.Lock()
	defer ns.Mu.Unlock()
	if nousConfigOutputMode(ns.Config) != nousOutputModeSpektacularRun {
		return nousRunApproval{}, nil
	}
	target, pending, err := nousPendingTarget(ns.Status)
	if err != nil {
		return nousRunApproval{handled: true}, &nousRunApprovalError{status: http.StatusBadRequest, err: err}
	}
	// Admit under the org-qualified repo, as inception approve does, so a
	// bare repo name keys the same run as its owner/name form.
	if s.deps.Config != nil {
		target.repo = config.QualifyRepo(s.deps.Config.Project.Org, target.repo)
	}
	if link := nousAdmittedRunLink(pending, target); link != nil {
		return nousRunApproval{link: link, handled: true, existing: true}, nil
	}
	if err := validateSpekHubRepoPath(target.repo); err != nil {
		return nousRunApproval{handled: true}, &nousRunApprovalError{status: http.StatusBadRequest, err: err}
	}
	if s.deps.Config == nil || !s.deps.Config.Runs.Spektacular.Enabled {
		return nousRunApproval{handled: true}, &nousRunApprovalError{status: http.StatusConflict, err: errors.New("runs.spektacular.enabled is required to admit run")}
	}
	if s.contributeHub == nil {
		return nousRunApproval{handled: true}, &nousRunApprovalError{status: http.StatusServiceUnavailable, err: errors.New("run lease registry unavailable")}
	}
	if err := s.AdmitRun(target.repo, target.number, target.title, time.Now()); err != nil {
		return nousRunApproval{handled: true}, &nousRunApprovalError{status: http.StatusInternalServerError, err: err}
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
	return nousRunApproval{link: link, handled: true}, nil
}

// nousAdmittedRunLink returns the run a pending proposal was already
// approved into, or nil when it has not been admitted. The link is read back
// from either its in-memory or its JSON-decoded form.
func nousAdmittedRunLink(pending map[string]interface{}, target nousRunTarget) map[string]string {
	if pending == nil {
		return nil
	}
	key := firstString(pending, "run_key")
	stage := ""
	switch run := pending["run"].(type) {
	case map[string]string:
		if key == "" {
			key = strings.TrimSpace(run["key"])
		}
		stage = strings.TrimSpace(run["stage"])
	case map[string]interface{}:
		if key == "" {
			key = firstString(run, "key")
		}
		stage = firstString(run, "stage")
	}
	if key == "" {
		return nil
	}
	if stage == "" {
		stage = StageSpec
	}
	return map[string]string{"key": key, "repo": target.repo, "number": strconv.Itoa(target.number), "stage": stage}
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

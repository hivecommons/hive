package hub

import (
	"fmt"
	"reflect"
	"strings"
	"time"
)

const (
	ghAppBucketOK       = "ok"
	ghAppBucketDegraded = "degraded"
	ghAppBucketBroken   = "broken"
	ghAppBucketUnknown  = "unknown"

	GitHubAppTokenStatusOK      = "ok"
	GitHubAppTokenStatusStale   = "stale"
	GitHubAppTokenStatusMissing = "missing"
	GitHubAppTokenStatusError   = "error"

	// GitHub App installation tokens expire after one hour. The spoke refreshes
	// them before expiry, so a cache minted more than 45 minutes ago means the
	// refresh path is already lagging and should be amber before agents hit 401s.
	GitHubAppTokenStaleAfter = 45 * time.Minute
)

type GitHubAppHealth struct {
	Bucket          string `json:"bucket"`
	Status          string `json:"status,omitempty"`
	State           string `json:"state,omitempty"`
	Host            string `json:"host,omitempty"`
	AppID           int64  `json:"appId,omitempty"`
	InstallationID  int64  `json:"installationId,omitempty"`
	HTTPStatus      int    `json:"httpStatus,omitempty"`
	ErrorClass      string `json:"errorClass,omitempty"`
	LastTokenMintAt string `json:"lastTokenMintAt,omitempty"`
	Detail          string `json:"detail,omitempty"`
}

func githubAppHealthFor(e RegistryEntry, now time.Time) GitHubAppHealth {
	out := GitHubAppHealth{
		Bucket:          ghAppBucketUnknown,
		Status:          strings.TrimSpace(e.GitHubAppTokenStatus),
		State:           strings.TrimSpace(e.GitHubAppState),
		Host:            githubAppHealthHost(e),
		AppID:           e.GitHubAppID,
		InstallationID:  e.GitHubInstallationID,
		HTTPStatus:      e.GitHubAppHTTPStatus,
		ErrorClass:      strings.TrimSpace(e.GitHubAppErrorClass),
		LastTokenMintAt: strings.TrimSpace(e.GitHubAppTokenLastMintAt),
		Detail:          strings.TrimSpace(e.GitHubAppTokenError),
	}
	if out.Detail == "" {
		out.Detail = githubAuthHealthDetail(e.Health)
	}
	if e.GitHubAppPermIssue != "" {
		out.Detail = e.GitHubAppPermIssue
	}
	if detail := githubAppStructuredDetail(out); detail != "" {
		out.Detail = detail
	}

	state := strings.TrimSpace(e.GitHubAppState)
	switch {
	case e.GitHubAppRequired || (state != "" && state != GitHubAppTokenStatusOK && state != "unknown"):
		out.Bucket = ghAppBucketBroken
		if out.Status == "" {
			out.Status = state
		}
		return out
	}
	switch out.Status {
	case GitHubAppTokenStatusOK:
		out.Bucket = ghAppBucketOK
	case GitHubAppTokenStatusStale:
		out.Bucket = ghAppBucketDegraded
	case GitHubAppTokenStatusMissing, GitHubAppTokenStatusError:
		out.Bucket = ghAppBucketBroken
	case "":
		if state == GitHubAppTokenStatusOK {
			out.Bucket = ghAppBucketOK
			out.Status = GitHubAppTokenStatusOK
		}
	default:
		out.Bucket = ghAppBucketUnknown
	}

	if out.Bucket == ghAppBucketOK && out.LastTokenMintAt != "" {
		if t, err := time.Parse(time.RFC3339, out.LastTokenMintAt); err == nil && now.Sub(t) > GitHubAppTokenStaleAfter {
			out.Bucket = ghAppBucketDegraded
			out.Status = GitHubAppTokenStatusStale
		}
	}
	return out
}

func githubAppHealthHost(e RegistryEntry) string {
	if h := strings.TrimSpace(e.GitHubHost); h != "" {
		return h
	}
	if h := strings.TrimSpace(e.GitHubAPIURL); h != "" {
		return githubHostLabel(h)
	}
	if h := strings.TrimSpace(e.GitHubBaseURL); h != "" {
		return githubHostLabel(h)
	}
	return ""
}

func githubAppStructuredDetail(h GitHubAppHealth) string {
	host := h.Host
	if host == "" {
		host = "GitHub"
	}
	switch strings.TrimSpace(h.ErrorClass) {
	case "installation-not-found":
		if h.AppID > 0 && h.InstallationID > 0 {
			return fmt.Sprintf("GitHub App %d on %s: installation %d not found (%d) — reinstall or fix app_id/installation_id",
				h.AppID, host, h.InstallationID, h.HTTPStatus)
		}
	case "key-invalid":
		if h.AppID > 0 {
			return fmt.Sprintf("GitHub App %d on %s: private key rejected — fix app_id or the installed private key",
				h.AppID, host)
		}
	case "key-missing":
		if h.AppID > 0 {
			return fmt.Sprintf("GitHub App %d on %s: private key missing — wait for hub delivery or reprovision the key",
				h.AppID, host)
		}
	case "wrong-installation":
		if h.AppID > 0 && h.InstallationID > 0 {
			return fmt.Sprintf("GitHub App %d on %s: installation %d belongs to a different account — fix installation_id",
				h.AppID, host, h.InstallationID)
		}
	}
	return ""
}

func githubAuthHealthDetail(health map[string]any) string {
	checks, ok := health["checks"]
	if !ok {
		return ""
	}
	v := reflect.ValueOf(checks)
	if v.Kind() != reflect.Slice && v.Kind() != reflect.Array {
		return ""
	}
	for i := 0; i < v.Len(); i++ {
		item := v.Index(i).Interface()
		if name, status, detail := healthCheckFields(item); name == "github_auth" && status == "fail" {
			return detail
		}
	}
	return ""
}

func healthCheckFields(item any) (name, status, detail string) {
	if m, ok := item.(map[string]any); ok {
		if m["detail"] == nil {
			return fmt.Sprint(m["name"]), fmt.Sprint(m["status"]), ""
		}
		return fmt.Sprint(m["name"]), fmt.Sprint(m["status"]), fmt.Sprint(m["detail"])
	}
	v := reflect.ValueOf(item)
	if v.Kind() == reflect.Pointer {
		v = v.Elem()
	}
	if v.Kind() != reflect.Struct {
		return "", "", ""
	}
	field := func(n string) string {
		f := v.FieldByName(n)
		if !f.IsValid() || f.Kind() != reflect.String {
			return ""
		}
		return f.String()
	}
	return field("Name"), field("Status"), field("Detail")
}

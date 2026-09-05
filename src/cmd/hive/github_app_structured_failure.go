package main

import (
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/hivecommons/hive/pkg/github"
)

var githubHTTPStatusRe = regexp.MustCompile(`\b([1-5][0-9]{2})\b`)

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func githubAppStructuredFailure(state, detail string) (class string, httpStatus int) {
	switch strings.TrimSpace(state) {
	case github.AppStateNotInstalled.String():
		class = "not-installed"
	case github.AppStateWrongInstallation.String():
		class = "wrong-installation"
	case github.AppStateInsufficientPerms.String():
		class = "insufficient-permissions"
	case github.AppStateKeyMissing.String():
		class = "key-missing"
	case github.AppStateKeyInvalid.String():
		class = "key-invalid"
	case github.AppStateNoAppAssigned.String():
		class = "no-app-assigned"
	case github.AppStateRepoNotCovered.String():
		class = "repo-not-covered"
	case github.AppStateRepoMoved.String():
		class = "repo-moved"
	case github.AppStateWriteForbidden.String():
		class = "write-forbidden"
	}
	if class == "" && strings.TrimSpace(detail) != "" {
		class = "token-error"
	}
	for _, m := range githubHTTPStatusRe.FindAllStringSubmatch(detail, -1) {
		if len(m) == 2 {
			if n, err := strconv.Atoi(m[1]); err == nil {
				httpStatus = n
			}
		}
	}
	if httpStatus != 0 {
		if (class == "token-error" || class == "not-installed") && httpStatus == http.StatusNotFound {
			class = "installation-not-found"
		}
	}
	return class, httpStatus
}

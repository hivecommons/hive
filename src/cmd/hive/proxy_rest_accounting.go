package main

import (
	"net/http"
	"time"

	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/crux/proxy"
)

// githubRESTAccounting adapts pkg/github's package-level REST accounting to
// the proxy's GitHubRESTAccounting seam, so the proxy (a Crux ring package)
// records relayed GitHub traffic and reads the shared core-bucket snapshot
// without importing pkg/github (hivecommons/hive#13536).
type githubRESTAccounting struct{}

var _ proxy.GitHubRESTAccounting = githubRESTAccounting{}

func (githubRESTAccounting) RecordRESTRequest(caller, method, path string, status int, h http.Header) {
	github.RecordRESTRequest(caller, method, path, status, h)
}

func (githubRESTAccounting) RecordInjectedRESTRequest(caller, method, path string, status int, h http.Header) {
	github.RecordInjectedRESTRequest(caller, method, path, status, h)
}

func (githubRESTAccounting) CoreRateLimitSnapshot() (remaining int, reset time.Time, ok bool) {
	return github.CoreRateLimitSnapshot()
}

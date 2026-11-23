// Fixture for hive-outbound-fetch-outside-netguard. Not compiled: semgrep
// --test parses it and checks each ruleid/ok annotation against the line
// below it.
package dashboard

import (
	"context"
	"encoding/json"
	"net"
	"net/http"

	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/netguard"
)

func proxyQueryParam(w http.ResponseWriter, r *http.Request) {
	target := r.URL.Query().Get("url")
	// ruleid: hive-outbound-fetch-outside-netguard
	resp, err := http.Get(target)
	if err != nil {
		return
	}
	defer resp.Body.Close()
}

func fetchFormValue(ctx context.Context, r *http.Request) error {
	// ruleid: hive-outbound-fetch-outside-netguard
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.FormValue("feed"), nil)
	if err != nil {
		return err
	}
	_, err = http.DefaultClient.Do(req)
	return err
}

type importRequest struct {
	URL string `json:"url"`
}

func importFromBody(r *http.Request) error {
	var body importRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		return err
	}
	// ruleid: hive-outbound-fetch-outside-netguard
	_, err := http.Get(body.URL)
	return err
}

func dialFromIssue(issue github.Issue) error {
	addr := firstLink(issue.Body)
	// ruleid: hive-outbound-fetch-outside-netguard
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		return err
	}
	return conn.Close()
}

func dialerFromComment(ctx context.Context, c github.ThreadComment) error {
	d := &net.Dialer{}
	// ruleid: hive-outbound-fetch-outside-netguard
	conn, err := d.DialContext(ctx, "tcp", c.Body)
	if err != nil {
		return err
	}
	return conn.Close()
}

// The guarded shape: classify first, then fetch through a pinned client.
func fetchChecked(ctx context.Context, r *http.Request, client *http.Client) error {
	target := r.URL.Query().Get("url")
	if netguard.IsPrivateURL(ctx, target, nil) {
		return errBlocked
	}
	// ok: hive-outbound-fetch-outside-netguard
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return err
	}
	_, err = client.Do(req)
	return err
}

// A fixed address is not user-chosen.
func fetchFixed(ctx context.Context) error {
	// ok: hive-outbound-fetch-outside-netguard
	_, err := http.Get("https://api.github.com/meta")
	return err
}

// A URL built from configuration, not from the request, is out of scope.
func fetchConfigured(base string) error {
	// ok: hive-outbound-fetch-outside-netguard
	_, err := http.Get(base + "/healthz")
	return err
}

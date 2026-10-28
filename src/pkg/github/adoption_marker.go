package github

import (
	"context"
	"strconv"
	"strings"

	gh "github.com/google/go-github/v72/github"
)

// SupersedeAdoption is the durable ownership evidence on the original PR.
type SupersedeAdoption struct {
	By   int
	Head string
}

// ReadSupersedeAdoption reads the latest hive-authored ownership marker, not
// merely the first adoption: a release invalidates earlier markers.
func ReadSupersedeAdoption(ctx context.Context, client *gh.Client, owner, repo string, number int, trusted func(string) bool) (SupersedeAdoption, error) {
	var latest SupersedeAdoption
	opts := &gh.ListOptions{PerPage: 100}
	for {
		comments, resp, err := client.Issues.ListComments(ctx, owner, repo, number, opts)
		if err != nil {
			return latest, err
		}
		for _, comment := range comments {
			if !trusted(comment.GetUser().GetLogin()) {
				continue
			}
			body := comment.GetBody()
			if strings.HasPrefix(body, "<!-- hive:adoption-reverted -->") || strings.HasPrefix(body, "<!-- hive:adoption-yield") {
				latest = SupersedeAdoption{}
				continue
			}
			if !strings.HasPrefix(body, "<!-- hive:adopted ") {
				continue
			}
			end := strings.Index(body, "-->")
			if end < 0 {
				continue
			}
			values := map[string]string{}
			for _, field := range strings.Fields(body[:end]) {
				k, v, ok := strings.Cut(field, "=")
				if ok {
					values[k] = v
				}
			}
			by, _ := strconv.Atoi(values["superseded_by"])
			latest = SupersedeAdoption{}
			if by > 0 && values["head"] != "" && values["mode"] == "supersede" {
				latest = SupersedeAdoption{By: by, Head: values["head"]}
			}
		}
		if resp == nil || resp.NextPage == 0 {
			return latest, nil
		}
		opts.Page = resp.NextPage
	}
}

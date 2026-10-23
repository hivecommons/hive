package github

import (
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// DependsOnVia marks a PRLinkedIssue parsed from a `Depends-on:` line.
const DependsOnVia = "depends-on"

// dependsOnLineRe matches one `Depends-on:` line of a PR body. The line may
// sit in a list or a quote; the keyword is case-insensitive and accepts
// "Depends on" as well.
var dependsOnLineRe = regexp.MustCompile(`(?im)^[ \t>*+-]*depends[- ]on[ \t]*:[ \t]*(.+)$`)

// dependsOnRefRe matches one reference on a `Depends-on:` line: `#N`,
// `owner/repo#N`, or a GitHub issue or pull request URL.
var dependsOnRefRe = regexp.MustCompile(`(?:https://github\.com/([\w.-]+/[\w.-]+)/(?:issues|pull)/|([\w.-]+/[\w.-]+)#|#)(\d+)`)

// ParseDependsOn returns the items a PR body explicitly says it depends on,
// one `Depends-on: #N` per line (a line may list several references). A bare
// `#N` refers to repo, the PR's own repository; `owner/repo#N` names another
// one. The PR author's declaration is the authoritative ordering edge the
// dashboard's your-turn critical path reads (hivecommons/hive#13646).
// Duplicates are dropped; the result is sorted by repository and number.
func ParseDependsOn(body, repo string) []PRLinkedIssue {
	if !strings.Contains(strings.ToLower(body), "depends") {
		return nil
	}
	seen := map[string]bool{}
	var out []PRLinkedIssue
	for _, line := range dependsOnLineRe.FindAllStringSubmatch(body, -1) {
		for _, m := range dependsOnRefRe.FindAllStringSubmatch(line[1], -1) {
			target := m[1]
			if target == "" {
				target = m[2]
			}
			if target == "" {
				target = repo
			}
			n, err := strconv.Atoi(m[3])
			if err != nil || n <= 0 {
				continue
			}
			key := strings.ToLower(target) + "#" + m[3]
			if seen[key] {
				continue
			}
			seen[key] = true
			out = append(out, PRLinkedIssue{Number: n, Repo: target, Via: DependsOnVia})
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if !strings.EqualFold(out[i].Repo, out[j].Repo) {
			return strings.ToLower(out[i].Repo) < strings.ToLower(out[j].Repo)
		}
		return out[i].Number < out[j].Number
	})
	return out
}

package github

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	gh "github.com/google/go-github/v72/github"
)

// A parked fix request often carries its change as text to apply ("replace
// this block with that one"). When another PR lands that same change, the
// park asks the owner to apply something already merged.
// Danathar/arch-bootc#634: ci-maintainer parked a retry for the build-cache
// delete loop in .github/workflows/build.yml and named the run-failure issue
// #633; nine minutes later scanner opened #635 ("closes #633") with the same
// retry in the same loop, and it merged. The re-check note said only "#633
// was closed", and the issue stayed in the owner's inbox.
//
// So for each item the park names that closed after the park — a merged PR,
// or an issue a merged PR closed — the re-check reads that PR's patch. When it
// removes a line the park quotes in a code block, in a file the park names,
// the change landed: the note says which PR and file, and the "What to reply"
// notice offers `/fixed` as the reply.

// parkRecheckLandedPrefix opens the hidden marker the note carries when it
// found the park's change landed; the unpark menu reads the PR from it.
const parkRecheckLandedPrefix = "<!-- hive:park-recheck-landed by="

// parkLandedMinLine is the shortest quoted line that counts as evidence; a
// `fi` or `exit 1` is removed by unrelated changes all the time.
const parkLandedMinLine = 16

// parkRecheckMaxLandedLookups bounds the named items a re-check reads a
// closer and patch for, per issue.
const parkRecheckMaxLandedLookups = 3

var parkFenceRE = regexp.MustCompile("(?s)```[^\\n]*\\n(.*?)```")

// parkQuotedLines returns the lines inside the park text's fenced code blocks,
// trimmed, that are long enough to be evidence.
func parkQuotedLines(text string) map[string]bool {
	out := map[string]bool{}
	for _, m := range parkFenceRE.FindAllStringSubmatch(text, -1) {
		for _, line := range strings.Split(m[1], "\n") {
			if l := strings.TrimSpace(line); len(l) >= parkLandedMinLine {
				out[l] = true
			}
		}
	}
	return out
}

// parkLandedFile returns a file the park names whose patch removes a line the
// park quotes, the first by name.
func parkLandedFile(text string, quoted map[string]bool, patches map[string]string) (string, bool) {
	files := make([]string, 0, len(patches))
	for f := range patches {
		files = append(files, f)
	}
	sort.Strings(files)
	for _, f := range files {
		if !strings.Contains(text, f) {
			continue
		}
		// GitHub's per-file patch starts at the first @@ hunk: it has no
		// ---/+++ file headers, so every "-" line is a removed line, one
		// that itself starts with "--" (a SQL comment) included.
		for _, line := range strings.Split(patches[f], "\n") {
			if !strings.HasPrefix(line, "-") {
				continue
			}
			if quoted[strings.TrimSpace(line[1:])] {
				return f, true
			}
		}
	}
	return "", false
}

// prFilePatches reads a PR's per-file patches.
func (c *Client) prFilePatches(ctx context.Context, owner, repo string, number int) (map[string]string, error) {
	opts := &gh.ListOptions{PerPage: 100}
	out := map[string]string{}
	for page := 0; page < claimSearchMaxPages; page++ {
		files, resp, err := c.client.PullRequests.ListFiles(ctx, owner, repo, number, opts)
		if err != nil {
			return nil, fmt.Errorf("listing files for PR %s/%s#%d: %w", owner, repo, number, err)
		}
		for _, file := range files {
			if file == nil || file.GetFilename() == "" {
				continue
			}
			out[file.GetFilename()] = file.GetPatch()
		}
		if resp == nil || resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}
	return out, nil
}

// markParkLandings sets landedBy/landedFile on the named changes whose
// change landed the park's quoted lines. A read that fails leaves the change
// as it was: the note then says only what moved, as before.
func (c *Client) markParkLandings(ctx context.Context, text string, refs map[string]parkRef, named []string, issueRepo string, start time.Time, changes []parkChange) {
	quoted := parkQuotedLines(text)
	if len(quoted) == 0 {
		return
	}
	byDisplay := map[string]int{}
	for i, ch := range changes {
		if ch.named && ch.copyOf == "" {
			byDisplay[ch.ref] = i
		}
	}
	lookups := 0
	for _, key := range named {
		ref, ok := refs[key]
		if !ok {
			continue
		}
		i, ok := byDisplay[parkRefDisplay(ref, issueRepo)]
		if !ok || lookups >= parkRecheckMaxLandedLookups {
			continue
		}
		owner, repo := c.splitRepo(ref.repo)
		pr := ref.number
		// One lookup per named item checked (a closer read for an issue,
		// then the patch read), so at most parkRecheckMaxLandedLookups
		// items cost reads.
		switch {
		case ref.isPR && ref.mergedAt.After(start):
			lookups++
		case !ref.isPR:
			lookups++
			closer, found, err := c.resolveIssueClosingPR(ctx, ref.repo, owner, repo, ref.number, 0)
			if err != nil || !found || !closer.MergedAt.After(start) {
				continue
			}
			pr = closer.Number
		default:
			continue
		}
		patches, err := c.prFilePatches(ctx, owner, repo, pr)
		if err != nil {
			continue
		}
		file, ok := parkLandedFile(text, quoted, patches)
		if !ok {
			continue
		}
		changes[i].landedFile = file
		if pr != ref.number {
			changes[i].landedBy = parkRefDisplay(parkRef{repo: ref.repo, number: pr}, issueRepo)
		} else {
			changes[i].landedBy = changes[i].ref
		}
	}
}

// parkRecheckLandedBy returns the PR a re-check note says landed the park's
// change, or "".
func parkRecheckLandedBy(body string) string {
	i := strings.Index(body, parkRecheckLandedPrefix)
	if i < 0 {
		return ""
	}
	rest := body[i+len(parkRecheckLandedPrefix):]
	end := strings.Index(rest, " -->")
	if end <= 0 {
		return ""
	}
	return strings.TrimSpace(rest[:end])
}

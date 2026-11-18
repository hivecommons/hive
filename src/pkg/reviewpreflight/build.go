package reviewpreflight

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/config"
)

const hoursPerDay = 24

// maxReasonLen caps an operator ignore reason echoed into the prompt.
const maxReasonLen = 200

var severityRank = map[string]int{"low": 1, "medium": 2, "high": 3, "critical": 4}

// Input is what the caller read from GitHub for one PR.
type Input struct {
	Repo       string
	Ref        string // base commit the tree was read at
	Tree       []TreeEntry
	TreeKnown  bool
	Files      []ChangedFile
	FilesKnown bool
}

func ecosystemByID(id string) (Ecosystem, bool) {
	for _, e := range Ecosystems {
		if e.ID == id {
			return e, true
		}
	}
	return Ecosystem{}, false
}

// Build returns the evidence prompt section for one PR, or "" when there is
// nothing to say. c may be nil when dependency checks are off.
func Build(ctx context.Context, c *Client, set config.ReviewPreflightResolved, in Input) string {
	var b strings.Builder
	if set.RuleDocs || len(set.Rules) > 0 {
		var sel RuleDocSelection
		if set.RuleDocs && in.TreeKnown {
			sel = SelectRuleDocs(in.Tree, set.RuleDocPatterns, set.RuleDocExclude, set.RuleDocBudgetBytes)
		}
		b.WriteString(renderRules(in.Repo, in.Ref, set, sel, in.TreeKnown))
	}
	if set.Dependencies {
		if c == nil {
			c = NewClient()
		}
		b.WriteString(dependencySection(ctx, c, set, in))
	}
	return b.String()
}

type depResult struct {
	dep        Dep
	advisories []Advisory
	accepted   []string
	published  time.Time
}

func dependencySection(ctx context.Context, c *Client, set config.ReviewPreflightResolved, in Input) string {
	if !in.FilesKnown {
		return "\nDEPENDENCY EVIDENCE: could not check: the PR's changed files could not be listed.\n"
	}
	var deps []Dep
	var notChecked []string
	touched := false
	for _, f := range in.Files {
		if !safePath(f.Path) {
			continue
		}
		if label, ok := unsupportedFor(f.Path); ok {
			touched = true
			notChecked = append(notChecked, fmt.Sprintf("%s (%s): no support for this manifest kind yet", f.Path, label))
			continue
		}
		eco, ok := ecosystemFor(f.Path)
		if !ok {
			continue
		}
		touched = true
		if strings.TrimSpace(f.Patch) == "" {
			notChecked = append(notChecked, fmt.Sprintf("%s (%s): no patch available (very large or binary diff)", f.Path, eco.Label))
			continue
		}
		deps = append(deps, eco.Parse(f.Path, patchLines(f.Patch))...)
	}
	if !touched {
		return ""
	}
	deps = dedupeDeps(deps)
	if set.MaxDependencies > 0 && len(deps) > set.MaxDependencies {
		for _, d := range deps[set.MaxDependencies:] {
			notChecked = append(notChecked, fmt.Sprintf("%s %s %s: over review.preflight.max_dependencies (%d)", d.Ecosystem, d.Name, d.Version, set.MaxDependencies))
		}
		deps = deps[:set.MaxDependencies]
	}

	var unavailable []string
	results := make([]depResult, len(deps))
	for i, d := range deps {
		results[i] = depResult{dep: d}
	}
	now := c.now()
	if set.Offline {
		unavailable = append(unavailable, "could not check: offline (review.preflight.offline or "+config.ReviewPreflightOfflineEnv+"); no vulnerability or release-age lookup was made")
	} else {
		total, cancel := context.WithTimeout(ctx, set.RequestTimeout*totalBudgetFactor)
		defer cancel()
		if set.Vulnerabilities {
			unavailable = append(unavailable, c.checkVulnerabilities(total, set, results, now)...)
		}
		if set.ReleaseAge && set.MinReleaseAgeDays > 0 {
			unavailable = append(unavailable, c.checkReleaseAge(total, set, results)...)
		}
	}
	return renderDeps(set, results, notChecked, unavailable, now)
}

func dedupeDeps(deps []Dep) []Dep {
	seen := map[string]bool{}
	var out []Dep
	for _, d := range deps {
		k := d.Ecosystem + "\x00" + d.Name + "\x00" + d.Version
		if seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, d)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Ecosystem != out[j].Ecosystem {
			return out[i].Ecosystem < out[j].Ecosystem
		}
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		return out[i].Version < out[j].Version
	})
	return out
}

func (c *Client) checkVulnerabilities(ctx context.Context, set config.ReviewPreflightResolved, results []depResult, now time.Time) []string {
	var idx []int
	var batch []Dep
	ids := map[string][]string{}
	for i, r := range results {
		e, ok := ecosystemByID(r.dep.Ecosystem)
		if !ok || e.OSV == "" || r.dep.Version == "" {
			continue
		}
		idx = append(idx, i)
		if v, hit := c.lookup("osvids\x00" + set.OSVURL + "\x00" + depKey(r.dep)); hit {
			ids[depKey(r.dep)] = v.([]string)
			continue
		}
		batch = append(batch, r.dep)
	}
	if len(idx) == 0 {
		return nil
	}
	if len(batch) > 0 {
		fresh, err := c.queryOSV(ctx, set.OSVURL, set.RequestTimeout, set.CacheTTL, batch)
		if err != nil {
			return []string{"could not check vulnerabilities: " + sanitizeLine(err.Error())}
		}
		for _, d := range batch {
			ids[depKey(d)] = fresh[depKey(d)]
			c.store("osvids\x00"+set.OSVURL+"\x00"+depKey(d), set.CacheTTL, fresh[depKey(d)])
		}
	}
	details := 0
	var notes []string
	for _, i := range idx {
		r := &results[i]
		for _, id := range ids[depKey(r.dep)] {
			adv := Advisory{ID: id}
			if details < maxAdvisoryDetails {
				details++
				d, derr := c.advisoryDetail(ctx, set.OSVURL, set.RequestTimeout, set.CacheTTL, id)
				if derr == nil {
					adv = d
				}
			}
			if adv.ID == "" {
				continue // withdrawn
			}
			if reason, hit, note := matchIgnore(set.Ignore, adv, r.dep.Name, now); hit {
				r.accepted = append(r.accepted, fmt.Sprintf("%s accepted by this hive (%s)", adv.ID, reason))
				continue
			} else if note != "" {
				notes = append(notes, note)
			}
			r.advisories = append(r.advisories, adv)
		}
	}
	return notes
}

func (c *Client) checkReleaseAge(ctx context.Context, set config.ReviewPreflightResolved, results []depResult) []string {
	failures := map[string]int{}
	firstErr := ""
	get := c.getter(set.RequestTimeout)
	checked := 0
	for i := range results {
		r := &results[i]
		e, ok := ecosystemByID(r.dep.Ecosystem)
		if !ok || e.Age == nil || r.dep.Version == "" {
			continue
		}
		base := e.Registry
		if o := set.RegistryURLs[e.ID]; o != "" {
			base = o
		}
		checked++
		v, err := c.remember("age\x00"+base+"\x00"+depKey(r.dep), set.CacheTTL, func() (any, error) {
			return e.Age(ctx, get, base, r.dep)
		})
		if err != nil {
			failures[e.ID]++
			if firstErr == "" {
				firstErr = sanitizeLine(err.Error())
			}
			continue
		}
		r.published = v.(time.Time)
	}
	if len(failures) == 0 {
		return nil
	}
	n := 0
	for _, f := range failures {
		n += f
	}
	return []string{fmt.Sprintf("could not check release age for %d of %d dependencies (first error: %s)", n, checked, firstErr)}
}

// matchIgnore applies the ignore list. hit is true when an unexpired entry
// accepts the advisory; note is set when a matching entry has expired.
func matchIgnore(ignores []config.ReviewPreflightIgnore, adv Advisory, pkg string, now time.Time) (reason string, hit bool, note string) {
	ids := append([]string{adv.ID}, adv.Aliases...)
	for _, ig := range ignores {
		if ig.Package != "" && !strings.EqualFold(ig.Package, pkg) {
			continue
		}
		match := false
		for _, id := range ids {
			if strings.EqualFold(ig.ID, id) {
				match = true
			}
		}
		if !match {
			continue
		}
		if ex := strings.TrimSpace(ig.Expires); ex != "" {
			if t, err := time.Parse(config.ReviewPreflightDateLayout, ex); err == nil && !now.Before(t.AddDate(0, 0, 1)) {
				note = fmt.Sprintf("ignore entry for %s expired on %s and no longer applies", sanitizeLine(ig.ID), ex)
				continue
			}
		}
		return truncate(sanitizeLine(ig.Reason), maxReasonLen), true, ""
	}
	return "", false, note
}

// ageIgnored reports whether the release-age note is suppressed for pkg.
func ageIgnored(ignores []config.ReviewPreflightIgnore, pkg string, now time.Time) bool {
	for _, ig := range ignores {
		if !strings.EqualFold(ig.ID, config.ReviewPreflightIgnoreReleaseAge) {
			continue
		}
		if ig.Package != "" && !strings.EqualFold(ig.Package, pkg) {
			continue
		}
		if ex := strings.TrimSpace(ig.Expires); ex != "" {
			if t, err := time.Parse(config.ReviewPreflightDateLayout, ex); err == nil && !now.Before(t.AddDate(0, 0, 1)) {
				continue
			}
		}
		return true
	}
	return false
}

func renderDeps(set config.ReviewPreflightResolved, results []depResult, notChecked, unavailable []string, now time.Time) string {
	var b strings.Builder
	b.WriteString("\nDEPENDENCY EVIDENCE (computed by the hive from this PR's changed manifests; identifiers, versions and dates only).\n")
	if len(results) == 0 {
		b.WriteString("No added or changed dependency versions were detected in the changed manifests.\n")
	}
	floor, block := severityRank[set.SeverityFloor], severityRank[set.BlockSeverity]
	for _, r := range results {
		d := r.dep
		fmt.Fprintf(&b, "- %s %s %s (%s):", d.Ecosystem, d.Name, orDash(d.Version), d.File)
		var parts []string
		if d.Note != "" {
			parts = append(parts, "not checked: "+d.Note)
		}
		var shown []string
		hidden := 0
		for _, a := range r.advisories {
			rank := severityRank[a.Severity]
			if a.Severity != "" && rank < floor {
				hidden++
				continue
			}
			label := a.ID
			switch {
			case a.Severity == "":
				label += " (severity not stated)"
			case rank >= block:
				label += " (" + a.Severity + ", at or above block_severity)"
			default:
				label += " (" + a.Severity + ")"
			}
			shown = append(shown, label)
		}
		if len(shown) > 0 {
			parts = append(parts, "advisories "+strings.Join(shown, ", "))
		} else if hidden > 0 {
			parts = append(parts, fmt.Sprintf("%d advisory record(s) below severity_floor not listed", hidden))
		} else if d.Note == "" && len(r.accepted) == 0 && set.Vulnerabilities && !set.Offline && !contains(unavailable, "could not check vulnerabilities") {
			if e, ok := ecosystemByID(d.Ecosystem); ok && e.OSV != "" && d.Version != "" {
				parts = append(parts, "no known advisories")
			}
		}
		parts = append(parts, r.accepted...)
		if !r.published.IsZero() {
			age := int(now.Sub(r.published).Hours() / hoursPerDay)
			s := fmt.Sprintf("published %s (%d days ago)", r.published.Format(config.ReviewPreflightDateLayout), age)
			if age < set.MinReleaseAgeDays && !ageIgnored(set.Ignore, d.Name, now) {
				s += fmt.Sprintf(", inside the %d day release-age window", set.MinReleaseAgeDays)
			}
			parts = append(parts, s)
		}
		if len(parts) == 0 {
			parts = append(parts, "no data")
		}
		b.WriteString(" " + strings.Join(parts, "; ") + "\n")
	}
	for _, s := range notChecked {
		b.WriteString("Not checked: " + sanitizeLine(s) + "\n")
	}
	for _, s := range unavailable {
		b.WriteString(sanitizeLine(s) + "\n")
	}
	b.WriteString("How to use this: report a vulnerable dependency only for an advisory id listed above, and cite the id. ")
	fmt.Fprintf(&b, "An advisory at or above %s is an in-scope finding (changes_requested); a lower one is an info or low note. ", set.BlockSeverity)
	if set.MinReleaseAgeDays > 0 {
		b.WriteString("A version inside the release-age window is a low or info supply-chain note with the publish date cited; never block on age alone. ")
	}
	b.WriteString("A line saying not checked or could not check means no data, not a clean result: say so rather than calling the dependency safe. Do not invent advisories or ages that are not listed.\n")
	return b.String()
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func contains(list []string, prefix string) bool {
	for _, s := range list {
		if strings.HasPrefix(s, prefix) {
			return true
		}
	}
	return false
}

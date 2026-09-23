package agentaudit

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"sort"
	"strings"

	"github.com/hivecommons/hive/pkg/timeline"
)

const (
	TrailerRun  = "Hive-Run"
	TrailerPlan = "Hive-Plan"
	TrailerSpec = "Hive-Spec"

	AuditArtifactLinkMissing = "artifact_link_missing"
)

var ErrNoLinkage = errors.New("commit has no hive artifact linkage")

type Links struct {
	SHA         string        `json:"sha"`
	Run         string        `json:"run,omitempty"`
	Plan        string        `json:"plan,omitempty"`
	Spec        string        `json:"spec,omitempty"`
	Clause      string        `json:"clause,omitempty"`
	PlanSection string        `json:"plan_section,omitempty"`
	Approval    *AuditRecord  `json:"approval,omitempty"`
	Rationale   []AuditRecord `json:"rationale"`
	Missing     []string      `json:"missing,omitempty"`
	NoLinkage   bool          `json:"no_linkage,omitempty"`
}

type AuditRecord struct {
	Timestamp string `json:"ts,omitempty"`
	User      string `json:"user,omitempty"`
	Action    string `json:"action,omitempty"`
	Detail    string `json:"detail,omitempty"`
	Agent     string `json:"agent,omitempty"`
}

type AuditReader interface {
	RecentAudit(n int) []AuditRecord
}

type TimelineReader interface {
	ByIssue(ref string) []timeline.Event
}

type CommitMessageReader interface {
	CommitMessage(ctx context.Context, sha string) (string, error)
}

type Resolver struct {
	GitDir   string
	Git      CommitMessageReader
	Timeline TimelineReader
	Audit    AuditReader
	Sink     AuditSink
}

func ResolveCommit(sha string) (Links, error) {
	return Resolver{}.ResolveCommit(context.Background(), sha)
}

func (r Resolver) ResolveCommit(ctx context.Context, sha string) (Links, error) {
	sha = strings.TrimSpace(sha)
	if sha == "" {
		return Links{}, fmt.Errorf("sha required")
	}
	reader := r.Git
	if reader == nil {
		reader = LocalGit{Dir: r.GitDir}
	}
	msg, err := reader.CommitMessage(ctx, sha)
	if err != nil {
		return Links{}, err
	}
	trailers := ParseTrailers(msg)
	links := Links{
		SHA:       sha,
		Run:       trailers[TrailerRun],
		Plan:      trailers[TrailerPlan],
		Rationale: []AuditRecord{},
	}
	links.Spec, links.Clause = splitSpecClause(trailers[TrailerSpec])
	links.Missing = missingArtifactTrailers(trailers)
	if len(links.Missing) > 0 {
		r.recordMissing(sha, links)
		if links.Run == "" {
			links.NoLinkage = true
			return links, nil
		}
	}
	if links.Run != "" && r.Timeline != nil {
		links.PlanSection = resolvePlanSection(r.Timeline.ByIssue(links.Run), links.Plan)
	}
	if r.Audit != nil {
		links.Approval, links.Rationale = resolveAuditLinks(r.Audit.RecentAudit(0), links.Run, links.Plan)
	}
	return links, nil
}

func (r Resolver) recordMissing(sha string, links Links) {
	if r.Sink == nil {
		return
	}
	fields := Fields("sha", sha, "missing", strings.Join(links.Missing, ","), "run", links.Run, "plan", links.Plan)
	if links.Spec != "" || links.Clause != "" {
		fields["clause"] = strings.Trim(strings.Join([]string{links.Spec, links.Clause}, "#"), "#")
	}
	r.Sink.Record("system", AuditArtifactLinkMissing, "", fields)
}

type LocalGit struct {
	Dir string
}

func (g LocalGit) CommitMessage(ctx context.Context, sha string) (string, error) {
	args := []string{"show", "-s", "--format=%B", "--no-patch", sha}
	cmd := exec.CommandContext(ctx, "git", args...)
	if g.Dir != "" {
		cmd.Dir = g.Dir
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("reading commit %s trailers: %w: %s", sha, err, strings.TrimSpace(stderr.String()))
	}
	return string(out), nil
}

func ParseTrailers(message string) map[string]string {
	out := map[string]string{}
	for _, line := range strings.Split(message, "\n") {
		key, val, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		switch key {
		case TrailerRun, TrailerPlan, TrailerSpec:
			out[key] = strings.TrimSpace(val)
		}
	}
	return out
}

func MissingTrailers(message string) []string {
	return missingArtifactTrailers(ParseTrailers(message))
}

func missingArtifactTrailers(trailers map[string]string) []string {
	required := []string{TrailerRun, TrailerPlan, TrailerSpec}
	var missing []string
	for _, k := range required {
		if strings.TrimSpace(trailers[k]) == "" {
			missing = append(missing, k)
		}
	}
	return missing
}

func splitSpecClause(raw string) (string, string) {
	spec, clause, ok := strings.Cut(strings.TrimSpace(raw), "#")
	if !ok {
		return strings.TrimSpace(raw), ""
	}
	return strings.TrimSpace(spec), strings.TrimSpace(clause)
}

func resolvePlanSection(events []timeline.Event, plan string) string {
	for _, ev := range events {
		if ev.Attrs == nil {
			continue
		}
		if plan != "" && ev.Attrs["plan"] != "" && ev.Attrs["plan"] != plan {
			continue
		}
		for _, k := range []string{"plan_section", "section", "plan_section_id"} {
			if v := strings.TrimSpace(ev.Attrs[k]); v != "" {
				return v
			}
		}
	}
	return ""
}

func resolveAuditLinks(entries []AuditRecord, run, plan string) (*AuditRecord, []AuditRecord) {
	var approval *AuditRecord
	var rationale []AuditRecord
	for i := range entries {
		fields := ParseDetailFields(entries[i].Detail)
		if run != "" && fields["run"] != run {
			continue
		}
		if plan != "" && fields["plan"] != "" && fields["plan"] != plan {
			continue
		}
		if strings.Contains(entries[i].Action, "approve") || strings.Contains(entries[i].Action, "approval") {
			cp := entries[i]
			if approval == nil || cp.Timestamp > approval.Timestamp {
				approval = &cp
			}
			continue
		}
		if entries[i].Action == AuditArtifactLinkMissing {
			continue
		}
		rationale = append(rationale, entries[i])
	}
	sort.SliceStable(rationale, func(i, j int) bool { return rationale[i].Timestamp < rationale[j].Timestamp })
	return approval, rationale
}

func ParseDetailFields(detail string) map[string]string {
	out := map[string]string{}
	for _, part := range strings.Split(detail, ",") {
		key, val, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok {
			continue
		}
		out[strings.TrimSpace(key)] = strings.TrimSpace(val)
	}
	return out
}

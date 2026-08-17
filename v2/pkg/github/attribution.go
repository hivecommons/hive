package github

import (
	"context"
	"log/slog"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"
)

// This file is the invocation-attribution trail for artifacts Hive creates on
// GitHub: agent PRs and issues opened by request watchers, plus Hive-owned
// advisory and dashboard ACMM-gap issues. It exists because
// an operator looking at an agent-authored PR previously had no way to answer
// "which backend/model produced this?" — the App-bot author is deterministic
// by design, so the invocation metadata must be recorded separately.
//
// Two layers, fed from the same launch-time metadata:
//
//  1. A VISIBLE one-line trailer appended to the artifact body at creation
//     time, gated by governor.attribution_trailer (default ON).
//  2. An AUDIT entry recorded for every such creation through the hooked
//     audit sink (the dashboard's audit.jsonl + ring buffer), written
//     regardless of the trailer toggle.
//
// Scope: only creations the hive MEDIATES are stamped. An issue an agent opens
// directly through its own CLI never passes through this code and is not
// covered — that limitation is intentional (the hive records what IT invoked,
// it does not rewrite artifacts it did not create).

// AttributionTrailerPrefix opens the visible trailer line. Neutral wording,
// stable and greppable — also the marker AppendTrailer uses to avoid stacking
// a second trailer onto a retried request.
const AttributionTrailerPrefix = "— hive:"

// Audit action names for creation events, following the audit log's
// snake_case action convention (cf. "config_governor_health").
const (
	// AuditActionAgentPRCreated is the audit action recorded when the
	// PR-request watcher opens a PR on an agent's behalf.
	AuditActionAgentPRCreated = "agent_pr_created"
	// AuditActionAgentIssueCreated is recorded when the issue-request watcher
	// creates or reuses an issue on an agent's behalf.
	AuditActionAgentIssueCreated = "agent_issue_created"
	// AuditActionHiveIssueCreated is the audit action recorded when the hive
	// itself creates an issue (advisory issue, ACMM-gap issue).
	AuditActionHiveIssueCreated = "hive_issue_created"
	// AuditActionPRMerged is the audit action recorded when the hive merges a
	// PR as the App bot over REST (MergePR). It closes the create→merge loop on
	// the dashboard audit log: issue created → PR created → PR merged.
	AuditActionPRMerged = "pr_merged"
	// AuditActionAdvisoryCommented is the audit action recorded when the hive
	// FIRST posts the advisory digest comment on the advisory issue. The digest
	// is refreshed (EditComment) roughly once a minute; those updates are NOT
	// audited — only the initial creation, a once-per-issue event.
	AuditActionAdvisoryCommented = "advisory_commented"
)

// System "agent" names recorded for creations no single coding agent
// performed. They occupy the same trailer/audit field as real agent names, so
// they must never collide with a configured agent name pattern in practice.
const (
	// AttributionAgentGovernor marks creations the governor/startup path makes
	// itself (e.g. the pinned advisory issue).
	AttributionAgentGovernor = "governor"
	// AttributionAgentDashboard marks creations triggered by an operator
	// action in the dashboard (e.g. ACMM-gap issues).
	AttributionAgentDashboard = "dashboard"
)

// backend ids / tool names used by the attribution mapping. These mirror the
// backend_binary() table in config/backends.conf — keep the two in sync.
const (
	backendBob     = "bob"      // IBM bobshell CLI; the binary is `bob`
	backendLiteLLM = "litellm"  // Claude Code pointed at a LiteLLM proxy
	toolBobshell   = "bobshell" // display name for the bob binary's version
	binaryClaude   = "claude"
	// bobAutoModel mirrors the dashboard's cli_models.go bobAutoModel: bob
	// exposes no model catalog and always self-selects, so "auto" is the
	// honest requested-model value. Discovering bob's INTERNAL routing (which
	// model actually served the session) is a known follow-up, deliberately
	// out of scope here.
	bobAutoModel = "auto"
)

// InvocationMeta is the launch-time metadata the hive knows about what it
// invoked. Fields left empty are OMITTED from the trailer and audit detail —
// the trail records what the hive actually knows, never a guess. It must only
// ever carry launch descriptors: no tokens, keys, or prompt content.
type InvocationMeta struct {
	// Agent is the hive agent name, or a system flow name (see
	// AttributionAgentGovernor / AttributionAgentDashboard).
	Agent string
	// Backend is the CLI backend the hive invoked (claude, copilot, bob, …),
	// including any runtime override in effect at launch.
	Backend string
	// Model is the model REQUESTED at launch. For backends that self-select
	// (bob), this is honestly "auto" — see RequestedModel.
	Model string
	// Tool is the display name for the version field (e.g. "bobshell"); the
	// trailer renders it as "<tool>=<version>".
	Tool string
	// ToolVersion is the CLI version as reported by `<binary> --version`.
	ToolVersion string
	// Session is a session/run identifier when the launch path has one.
	// Currently unset: the spoke's tmux session name is just the agent name,
	// so recording it would add no information. The field exists so a real
	// per-run id can flow through without a format change.
	Session string
}

// pairs returns the known metadata as ordered key/value pairs, omitting
// unknown (empty) fields.
func (m InvocationMeta) pairs() []string {
	var kv []string
	add := func(k, v string) {
		if v = strings.TrimSpace(v); v != "" {
			kv = append(kv, k, v)
		}
	}
	add("agent", m.Agent)
	add("backend", m.Backend)
	add("model", m.Model)
	if ver := strings.TrimSpace(m.ToolVersion); ver != "" {
		tool := strings.TrimSpace(m.Tool)
		if tool == "" {
			tool = "tool"
		}
		kv = append(kv, tool, ver)
	}
	add("session", m.Session)
	return kv
}

// Trailer renders the one-line visible trailer, e.g.:
//
//	— hive: agent=quality backend=bob model=auto bobshell=1.0.6 session=abc123
//
// Unknown fields are omitted; all-unknown metadata renders "" (no trailer at
// all rather than a bare prefix).
func (m InvocationMeta) Trailer() string {
	kv := m.pairs()
	if len(kv) == 0 {
		return ""
	}
	parts := make([]string, 0, len(kv)/2)
	for i := 0; i+1 < len(kv); i += 2 {
		parts = append(parts, kv[i]+"="+kv[i+1])
	}
	return AttributionTrailerPrefix + " " + strings.Join(parts, " ")
}

// AuditDetail renders the audit-entry detail string in the audit log's
// "k=v, k=v" convention (see dashboard auditDetail): the artifact coordinates
// first (extra pairs, e.g. "repo", "org/x", "number", "12", "url", …), then
// the invocation metadata.
func (m InvocationMeta) AuditDetail(extra ...string) string {
	kv := append(append([]string{}, extra...), m.pairs()...)
	parts := make([]string, 0, len(kv)/2)
	for i := 0; i+1 < len(kv); i += 2 {
		parts = append(parts, kv[i]+"="+kv[i+1])
	}
	return strings.Join(parts, ", ")
}

// AppendTrailer appends the visible trailer to body, blank-line separated.
// No-op when the trailer is empty or the body already carries one — a request
// the watcher retries after a partial failure must not stack trailers.
func AppendTrailer(body string, m InvocationMeta) string {
	t := m.Trailer()
	if t == "" || strings.Contains(body, AttributionTrailerPrefix) {
		return body
	}
	body = strings.TrimRight(body, "\n")
	if body == "" {
		return t
	}
	return body + "\n\n" + t
}

// RequestedModel normalizes the model recorded in the trail: bob has no model
// catalog and always self-selects, so an empty model on the bob backend is
// honestly "auto" rather than unknown. Every other backend passes through
// unchanged (empty stays empty and is omitted, never guessed).
func RequestedModel(backend, model string) string {
	if backend == backendBob && strings.TrimSpace(model) == "" {
		return bobAutoModel
	}
	return model
}

// toolVersionTimeout bounds the one-shot `<binary> --version` probe so a hung
// CLI can never stall the PR-open path.
const toolVersionTimeout = 5 * time.Second

// toolVersionPattern extracts the version token (e.g. "1.0.6") from CLI
// --version output, which varies in shape across backends.
var toolVersionPattern = regexp.MustCompile(`\d+\.\d+(\.\d+)?`)

// toolVersionCache caches binary → version ("" = probe failed) per process.
// CLIs change only via image rebuild or an in-place upgrade followed by an
// agent relaunch; either way the next hive restart refreshes the cache.
var toolVersionCache sync.Map

// backendToolBinary maps a backend id to its (display tool name, binary).
// Mirrors backend_binary() in config/backends.conf — keep the two in sync.
// Unknown/inference backends map to themselves; a missing binary simply
// resolves to no version, which the trailer omits.
func backendToolBinary(backend string) (tool, binary string) {
	switch backend {
	case backendBob:
		return toolBobshell, backendBob
	case backendLiteLLM:
		// litellm agents run the Claude CLI pointed at a LiteLLM proxy.
		return binaryClaude, binaryClaude
	case "":
		return "", ""
	default:
		return backend, backend
	}
}

// ResolveToolVersion best-effort resolves the CLI version for a backend by
// running `<binary> --version` once per process (cached). Returns ("", "")
// when unknown; callers omit the field rather than guessing.
func ResolveToolVersion(backend string) (tool, version string) {
	tool, binary := backendToolBinary(backend)
	if binary == "" {
		return "", ""
	}
	if v, ok := toolVersionCache.Load(binary); ok {
		return tool, v.(string)
	}
	ctx, cancel := context.WithTimeout(context.Background(), toolVersionTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, binary, "--version").CombinedOutput()
	if err == nil {
		version = toolVersionPattern.FindString(string(out))
	}
	toolVersionCache.Store(binary, version)
	return tool, version
}

// AttributionHooks are the seams main() wires so this package can stamp and
// record creations without importing the agent or dashboard packages (both
// import this one). Each hook is optional and becomes available at a
// different point during startup — see the Set* methods.
type AttributionHooks struct {
	// Resolve returns launch metadata for a named agent (effective backend/
	// model from the agent manager plus tool version). nil → metadata beyond
	// the agent name is simply unknown and omitted.
	Resolve func(agent string) InvocationMeta
	// TrailerEnabled gates ONLY the visible trailer. nil → enabled (the
	// config default is ON). Read per-creation so a dashboard toggle takes
	// effect immediately, no restart.
	TrailerEnabled func() bool
	// Audit records the audit entry (action, detail, agent) — wired to the
	// dashboard audit log (audit.jsonl + ring). It is called for EVERY
	// mediated creation regardless of the trailer toggle. nil → the creation
	// is still recorded in the hive log via slog so the trail never fully
	// disappears (only the pre-dashboard startup window can hit this).
	Audit func(action, detail, agent string)
}

// SetAttributionHooks installs the initial hook set. Safe to call before the
// watcher starts; individual hooks can be added later via SetAttribution
// Resolver / SetAttributionAudit as their dependencies come up.
func (c *Client) SetAttributionHooks(h AttributionHooks) {
	if c == nil {
		return
	}
	c.attribMu.Lock()
	defer c.attribMu.Unlock()
	c.attribution = h
}

// SetAttributionResolver installs the per-agent metadata resolver (available
// once the agent manager exists).
func (c *Client) SetAttributionResolver(fn func(agent string) InvocationMeta) {
	if c == nil {
		return
	}
	c.attribMu.Lock()
	defer c.attribMu.Unlock()
	c.attribution.Resolve = fn
}

// SetAttributionAudit installs the audit sink (available once the dashboard
// server exists).
func (c *Client) SetAttributionAudit(fn func(action, detail, agent string)) {
	if c == nil {
		return
	}
	c.attribMu.Lock()
	defer c.attribMu.Unlock()
	c.attribution.Audit = fn
}

// attributionMeta resolves launch metadata for an agent name, degrading to
// name-only metadata when no resolver is wired.
func (c *Client) attributionMeta(agent string) InvocationMeta {
	if c == nil {
		return InvocationMeta{Agent: agent}
	}
	c.attribMu.RLock()
	resolve := c.attribution.Resolve
	c.attribMu.RUnlock()
	if resolve == nil {
		return InvocationMeta{Agent: agent}
	}
	m := resolve(agent)
	if m.Agent == "" {
		m.Agent = agent
	}
	return m
}

// attributionTrailerOn reports whether the visible trailer should be stamped.
// Default ON when no hook is wired, matching the config default.
func (c *Client) attributionTrailerOn() bool {
	if c == nil {
		return true
	}
	c.attribMu.RLock()
	enabled := c.attribution.TrailerEnabled
	c.attribMu.RUnlock()
	if enabled == nil {
		return true
	}
	return enabled()
}

// recordCreationAudit writes the unconditional audit entry for a mediated
// creation. With no sink wired yet (pre-dashboard startup window) it falls
// back to the hive log so the event is still recorded somewhere durable.
func (c *Client) recordCreationAudit(action string, m InvocationMeta, extra ...string) {
	if c == nil {
		return
	}
	detail := m.AuditDetail(extra...)
	c.attribMu.RLock()
	audit := c.attribution.Audit
	c.attribMu.RUnlock()
	if audit != nil {
		audit(action, detail, m.Agent)
		return
	}
	c.logger.Info("attribution audit (no audit sink wired yet)",
		slog.String("action", action), slog.String("detail", detail), slog.String("agent", m.Agent))
}

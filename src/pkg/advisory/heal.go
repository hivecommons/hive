package advisory

import (
	"regexp"
	"strings"

	"github.com/hivecommons/hive/pkg/beads"
)

// appAuthHealedCloseReason is stamped into a bead's metadata when the hive
// auto-closes it because the GitHub App demonstrably works. Kept as a named
// constant so logs, tests and the bead trail all agree on the exact wording.
const appAuthHealedCloseReason = "auto-closed: the GitHub App successfully posted the advisory digest, so this access finding no longer holds"

// closeReasonMetadataKey is the metadata key used to record why a bead was
// auto-closed. It matches the key beads.CloseAll already writes so every
// close path is inspectable the same way.
const closeReasonMetadataKey = "close_reason"

// appAuthFindingPatterns recognize advisory findings that describe THIS hive's
// own GitHub access — the App not being installed, or the installation lacking
// repo permissions. These are environment conditions the hive can verify
// itself, unlike code findings, which only an agent can re-check.
//
// The patterns are deliberately narrow: they must match agent phrasings like
// "Insufficient repo permissions" or "GitHub App not installed" without ever
// matching a CODE finding that merely mentions permissions (e.g. "Workflow
// permissions overly broad in ci.yml" or "Add permission checks to admin
// API"). A false close here would silently retire a real repo-content finding,
// so every pattern requires the insufficiency/installation language, not just
// the word "permission".
var appAuthFindingPatterns = []*regexp.Regexp{
	// "Insufficient repo permissions", "insufficient permissions for the app"
	regexp.MustCompile(`(?i)\binsufficient\b[^.\n]*\bpermissions?\b`),
	// "Repo permissions insufficient", "permissions are insufficient"
	regexp.MustCompile(`(?i)\bpermissions?\b[^.\n]*\binsufficient\b`),
	// "GitHub App not installed", "GitHub App lacks repo permissions"
	regexp.MustCompile(`(?i)\bgithub app\b[^.\n]*\b(not installed|missing|lacks|permissions?|access)\b`),
	// The literal 403 body GitHub returns when an installation token cannot
	// act on a repo — agents quote it verbatim in findings.
	regexp.MustCompile(`(?i)resource not accessible by integration`),
}

// IsAppAuthFinding reports whether a finding title describes the hive's own
// GitHub App access (installation / repo permissions) rather than a code
// finding. Only titles — never free-form details — are matched, because the
// title is what PersistAsBeads dedups on and what the digest displays.
func IsAppAuthFinding(title string) bool {
	if title == "" {
		return false
	}
	for _, p := range appAuthFindingPatterns {
		if p.MatchString(title) {
			return true
		}
	}
	return false
}

// CloseHealedAppAuthFindings closes every open advisory-digest bead that
// describes the hive's GitHub App access, across all agent stores. Callers
// invoke it ONLY after the App has PROVEN it can write — a successful
// App-authenticated advisory-digest post — so a finding is never closed on a
// guess (#2575: an "Insufficient repo permissions" bead survived forever after
// the App was correctly installed, because nothing ever re-validated it).
//
// Beads that are already closed/done are left untouched, as are findings that
// do not match the app-auth patterns — a genuinely-failing permission
// condition never reaches this function, because the digest post that gates it
// would have failed. Returns the closed titles for logging.
func CloseHealedAppAuthFindings(stores map[string]*beads.Store) []string {
	var closed []string
	for _, store := range stores {
		for _, b := range store.List(beads.ListFilter{}) {
			if b.Status == beads.StatusClosed || b.Status == beads.StatusDone {
				continue
			}
			if !isAdvisoryBeadType(b.Type) {
				continue
			}
			if !IsAppAuthFinding(b.Title) {
				continue
			}
			if err := store.Close(b.ID); err != nil {
				continue
			}
			_ = store.SetMetadata(b.ID, closeReasonMetadataKey, appAuthHealedCloseReason)
			closed = append(closed, b.Title)
		}
	}
	return closed
}

// repoAccessHealedCloseReason is stamped into a bead's metadata when the hive
// auto-closes it because the advisory-tier repository READ path demonstrably
// works. Distinct from appAuthHealedCloseReason because the proof is different:
// a digest post proves the App can WRITE issues, while this close requires an
// actual verified Contents read.
const repoAccessHealedCloseReason = "auto-closed: the hive verified an advisory-tier scoped token can read the repository, so this repo-access finding no longer holds"

// repoAccessFindingPatterns recognize the #2575 finding family: advisory-tier
// (guide/newcomer) agents reporting they have NO WAY TO READ the repository
// they exist to audit — "no clone mechanism", "no repository access mechanism
// in L2 advisory mode", "repository access infrastructure missing", "lacks
// read-only repository access", "no repository workspace provisioning". These
// were true findings before #4291 (the advisor/newcomer scoped-token tiers
// omitted Contents:read and the credential helper blocked fetches), and agents
// word them freely, so the patterns tolerate drift.
//
// "Tolerate drift" was not tolerant enough (#4464): a hive's digest showed two
// findings of exactly this family that no pattern matched, so the healer never
// even considered them and they sat in the digest permanently —
//
//	"Repository worktree not provisioned for guide agent despite
//	 include_repos=true configuration"
//	"Quality agent lacks read access to repository for coverage analysis"
//
// Two gaps, both about word ORDER rather than vocabulary. The first states the
// claim in the passive past participle ("worktree not provisioned") where the
// provisioning pattern below only read the noun form ("no ... provisioning"),
// and says "worktree" where the pattern said "workspace". The second writes
// "read access TO repository" where the access patterns require "repository
// ... access". Both are ordinary English an agent will produce, so the last
// two patterns cover them explicitly.
//
// Like appAuthFindingPatterns they must stay directional: every pattern
// requires the LACK language ("no/missing/lacks/cannot", or "not/never
// provisioned") adjacent to the access/clone subject, so a CODE finding that
// merely mentions repository access ("Repository access logs are not
// retained", "read access to repository secrets not restricted") never
// matches. And unlike the app-auth
// healer, a title match alone is NOT sufficient to close — see
// CloseHealedRepoAccessFindings, which additionally requires a verified read
// of the repository the finding concerns.
var repoAccessFindingPatterns = []*regexp.Regexp{
	// "no clone mechanism available", "agent lacks a checkout mechanism"
	regexp.MustCompile(`(?i)\b(no|missing|lacks?|without|unavailable)\b[^.\n]*\b(clone|checkout|fetch)\s+mechanism\b`),
	// "clone mechanism missing/unavailable/not available"
	regexp.MustCompile(`(?i)\b(clone|checkout|fetch)\s+mechanism\b[^.\n]*\b(missing|unavailable|absent|not\s+available)\b`),
	// "no repository access mechanism", "lacks read-only repository access
	// mechanism", "no repo read access path"
	regexp.MustCompile(`(?i)\b(no|missing|lacks?|without|unavailable)\b[^.\n]*\brepo(sitory)?\s+(read\s+)?access\b[^.\n]*\b(mechanism|infrastructure|capabilit|path|method|provision)`),
	// "repository access infrastructure missing"
	regexp.MustCompile(`(?i)\brepo(sitory)?\s+(read\s+)?access\b[^.\n]*\b(mechanism|infrastructure|capabilit|path|method|provision)[a-z]*\b[^.\n]*\b(missing|unavailable|absent|lacking|not\s+available)\b`),
	// "no repository workspace provisioning for advisory agents", "no git
	// worktree provisioned", "missing repository checkout provisioning"
	regexp.MustCompile(`(?i)\b(no|missing|lacks?|without)\b[^.\n]*\b(repo(sitory)?|git|agent)\s+(workspace|work\s?tree|checkout|clone)\s+provision`),
	// "guide agent cannot access target repository", "unable to clone the repository"
	regexp.MustCompile(`(?i)\b(cannot|can't|unable\s+to)\s+(access|read|clone|fetch)\b[^.\n]*\brepo(sitory)?\b`),
	// #4464, gap 1: the same claim in the passive — "Repository worktree not
	// provisioned for guide agent", "agent workspace never provisioned". The
	// pattern above needs a leading lack-word and the noun "provisioning";
	// this one needs neither, so it requires the agent-infrastructure subject
	// (repo/git/agent + workspace/worktree/checkout/clone) to stay directional.
	regexp.MustCompile(`(?i)\b(repo(sitory)?|git|agent)\s+(workspace|work\s?tree|checkout|clone)\b[^.\n]*\b(not|never)\s+provisioned\b`),
	// #4464, gap 2: "lacks read access TO repository", "no clone access to the
	// repo" — the reverse word order of the access patterns above, which only
	// read "repository ... access". WRITE access is deliberately NOT listed:
	// the close below is gated on a verified READ, which would be the wrong
	// proof for a write-access claim (those are the app-auth healer's).
	regexp.MustCompile(`(?i)\b(no|missing|lacks?|without|unavailable)\b[^.\n]*\b(read-only|read|clone|checkout|fetch)\s+access\s+to\b[^.\n]*\brepo(sitory)?\b`),
	// #4464, gap 3 (found live on a customer hive AFTER the first two fixes
	// shipped): "Repository not provisioned for guide agent - cannot perform
	// documentation audit". The bare repo noun with no workspace/worktree/
	// checkout qualifier — gap 1's pattern requires that middle noun, so this
	// escaped. Directional: the "not/never provisioned" claim must attach to
	// the repo(sitory) subject within the same clause.
	regexp.MustCompile(`(?i)\brepo(sitory)?\b[^.\n]*\b(not|never|un)provisioned\b|\brepo(sitory)?\b[^.\n]*\b(not|never)\s+provisioned\b`),
}

// IsRepoAccessFinding reports whether a finding title describes the hive's own
// agents lacking a repository read path (clone/fetch/contents access), as
// opposed to a code finding that merely mentions repository access. Titles
// only, for the same reason as IsAppAuthFinding.
func IsRepoAccessFinding(title string) bool {
	if title == "" {
		return false
	}
	for _, p := range repoAccessFindingPatterns {
		if p.MatchString(title) {
			return true
		}
	}
	return false
}

// RepoFromFindingRef extracts an "owner/repo" from a finding's file reference
// (Bead.ExternalRef), when the agent attributed the finding to a repository
// rather than a file. Agents write refs like:
//
//	"github.ibm.com/devx-prod/epx-vscode-ext-poc" → devx-prod/epx-vscode-ext-poc
//	"hive.yaml:repos", "src/main.go:12", "hive-infrastructure" → not a repo
//
// The parse is deliberately conservative: anything with a ":" is a file:line
// or file:section ref, and a two-segment path only parses as owner/repo when
// neither segment contains a "." (so "docs/install.md" never does). A ref
// that fails to parse means "the finding is about the hive's own configured
// repo", which is the safe default for the verification gate.
func RepoFromFindingRef(ref string) (string, bool) {
	ref = strings.TrimSpace(ref)
	if ref == "" || strings.Contains(ref, ":") {
		return "", false
	}
	parts := strings.Split(strings.Trim(ref, "/"), "/")
	switch len(parts) {
	case 3:
		// host/owner/repo — the leading segment must look like a hostname.
		if strings.Contains(parts[0], ".") && parts[1] != "" && parts[2] != "" {
			return parts[1] + "/" + parts[2], true
		}
	case 2:
		if parts[0] != "" && parts[1] != "" && !strings.Contains(parts[0], ".") && !strings.Contains(parts[1], ".") {
			return parts[0] + "/" + parts[1], true
		}
	}
	return "", false
}

// CloseHealedRepoAccessFindings closes open advisory beads that report the
// hive's agents lacking a repository READ path, but ONLY for repositories the
// caller can PROVE are readable right now. canRead receives the "owner/repo"
// the finding names (parsed from its ref), or "" when the finding names no
// repo — meaning the hive's own primary repo. It must return true only after
// an actual verified read (e.g. a Contents fetch with an advisory-tier scoped
// token — the exact path #4291 fixed), and is consulted per bead so a finding
// about a repo that is GENUINELY unreadable is never closed alongside healed
// ones. Callers should memoize canRead: many findings name the same repo.
//
// Invoked after a successful App-authenticated digest post, like
// CloseHealedAppAuthFindings — but the digest post only proves issues:write,
// so the read verification is what actually gates each close here. A wrongly
// surviving finding costs nothing (it heals next cycle); a wrongly closed one
// re-opens only when an agent re-files it, so the gate errs toward staying
// open. Returns the closed titles for logging.
func CloseHealedRepoAccessFindings(stores map[string]*beads.Store, canRead func(ownerRepo string) bool) []string {
	if canRead == nil {
		return nil
	}
	var closed []string
	for _, store := range stores {
		if store == nil {
			continue
		}
		for _, b := range store.List(beads.ListFilter{}) {
			if b.Status == beads.StatusClosed || b.Status == beads.StatusDone {
				continue
			}
			if !isAdvisoryBeadType(b.Type) {
				continue
			}
			if !IsRepoAccessFinding(b.Title) {
				continue
			}
			repo, _ := RepoFromFindingRef(b.ExternalRef)
			if !canRead(repo) {
				continue
			}
			if err := store.Close(b.ID); err != nil {
				continue
			}
			_ = store.SetMetadata(b.ID, closeReasonMetadataKey, repoAccessHealedCloseReason)
			closed = append(closed, b.Title)
		}
	}
	return closed
}

// prLinkedCloseReason is stamped into a bead's metadata when a merged PR
// retires it.
const prLinkedCloseReason = "auto-closed: a merged pull request addresses this finding"

// prLinkThreshold is the Jaccard title similarity at or above which a merged
// PR is taken to address an open advisory finding.
//
// It sits BELOW nearDuplicateThreshold (0.5) deliberately: that threshold
// compares two statements of the same problem by the same kind of author, while
// this compares a finding ("pr-verifier.yml fails on every PR") against a human
// PR title ("fix pr-verifier workflow"), which shares fewer words by nature.
// The cost asymmetry is the opposite of dedup's, too — a wrongly closed finding
// re-opens the moment an agent re-files it (Upsert creates a fresh bead once
// the old one is closed), whereas a finding that survives its own fix is the
// permanent staleness this refactor exists to remove.
const prLinkThreshold = 0.4

// ClosePRLinkedAdvisoryBeads closes open advisory beads whose title is
// sufficiently similar to the title of a recently merged PR, returning the
// closed titles for logging.
//
// Called only after a PR is VERIFIED merged, so a fix that addresses a finding
// retires it from the digest automatically instead of waiting out the staleness
// window.
func ClosePRLinkedAdvisoryBeads(stores map[string]*beads.Store, prTitle string) []string {
	prTokens := findingTokens(prTitle)
	if len(prTokens) == 0 {
		return nil
	}
	var closed []string
	for _, store := range stores {
		if store == nil {
			continue
		}
		for _, b := range store.List(beads.ListFilter{}) {
			if b.Status == beads.StatusClosed || b.Status == beads.StatusDone {
				continue
			}
			// TypeAdvisory only, deliberately narrower than the digest's
			// isAdvisoryBeadType: bug and feature beads are PLANNED WORK an
			// agent or operator filed, and retiring one on a title resemblance
			// would delete work nobody agreed was done. An advisory bead is a
			// report of a condition, which a merged fix genuinely can end.
			if b.Type != beads.TypeAdvisory {
				continue
			}
			if jaccard(findingTokens(b.Title), prTokens) < prLinkThreshold {
				continue
			}
			if err := store.Close(b.ID); err != nil {
				continue
			}
			_ = store.SetMetadata(b.ID, closeReasonMetadataKey, prLinkedCloseReason)
			closed = append(closed, b.Title)
		}
	}
	return closed
}

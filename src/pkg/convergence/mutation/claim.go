// Package mutation implements the fenced mutation ownership and idempotent
// operation journal accepted on kubestellar/hive#4255 (parent epic #3845,
// issue #4256), for exactly ONE mutation class: contributor task-lease-backed
// repository mutations, with ONE external effect named by the accepted C2
// binding — GitHub pull-request creation for the assigned task.
//
// It owns two durable truths and deliberately nothing more:
//
//   - the claim ledger: WHO currently owns a semantic mutation resource, at
//     WHICH monotonically increasing per-claim epoch, in WHICH lifecycle
//     state — the durable authority the in-memory contributor lease
//     (contribute_ws.go taskLease) never was; and
//
//   - the operation journal: for each logical operation — identified WITHOUT
//     owner or epoch, so retry or reassignment of the same desired effect
//     always finds the same entry — whether its external effect was planned,
//     applied, not applied, or left uncertain, with every authorized attempt
//     and its owner epoch recorded inside the entry.
//
// Only the current durable claim epoch may authorize or acknowledge the
// external effect, validated at the actual mutation boundary and again before
// acknowledgment. After a timeout, crash, restart, or reassignment, current
// authoritative external state reconciles the SAME logical operation —
// Applied, NotApplied, or Unknown — before any retry; a replacement owner
// adopts the existing operation under its newer epoch and can never mint a
// second logical ID for unchanged effect inputs.
//
// The package is inert by default: nothing creates, reads, or consults a
// ledger or journal unless the convergence mode toggle (convergence.mode /
// HIVE_CONVERGENCE_MODE) resolves to a non-off mode, and only exact enforce
// mode fences — shadow records and reports but never withholds an effect.
// Existing PR-creation dedupe and exact-head merge guards remain mandatory
// defense-in-depth the journal never replaces.
package mutation

import (
	"fmt"
	"strings"
)

// Claim types, exactly the closed set accepted on #4255. Adding a type is a
// new accepted contract, never a silent extension.
const (
	// ClaimTypeRepo is the repository-wide/top claim: it conflicts with every
	// claim in the same canonical repository, so a broad mutation can never
	// bypass narrower claims.
	ClaimTypeRepo = "repo"
	// ClaimTypeTask claims one work item's mutation: the canonical
	// source-aware subject key ("owner/repo#N" or "owner/repo!KEY").
	ClaimTypeTask = "task"
)

// claimKeyPrefix namespaces claim keys apart from every other key family in
// Hive ("#", "!", "@" work/outcome keys never carry a "mutation:" prefix).
const claimKeyPrefix = "mutation:"

// Claim is the canonical semantic mutation resource accepted on #4255.
// Claims are only ever minted from the hub's own lease tuple and
// worksource.Ref.Key() — never from client-supplied strings — which is what
// keeps spelling aliases from bypassing overlap.
type Claim struct {
	// Type is ClaimTypeRepo or ClaimTypeTask.
	Type string `json:"type"`
	// Repo is the canonical owner/repo spelling the lease records.
	Repo string `json:"repo"`
	// Subject is empty for a repo claim, and the canonical work key
	// (worksource.Ref.Key()) for a task claim.
	Subject string `json:"subject,omitempty"`
}

// Validate reports why a Claim cannot serve as a canonical resource identity.
func (c Claim) Validate() error {
	if c.Repo == "" || strings.Count(c.Repo, "/") != 1 || strings.ContainsAny(c.Repo, "@ \t") {
		return fmt.Errorf("claim repo %q is not a canonical owner/repo spelling", c.Repo)
	}
	switch c.Type {
	case ClaimTypeRepo:
		if c.Subject != "" {
			return fmt.Errorf("a repo claim carries no subject, got %q", c.Subject)
		}
	case ClaimTypeTask:
		if c.Subject == "" {
			return fmt.Errorf("a task claim requires a canonical subject key")
		}
		if !strings.HasPrefix(c.Subject, c.Repo) || len(c.Subject) <= len(c.Repo) {
			return fmt.Errorf("task subject %q does not belong to repo %q", c.Subject, c.Repo)
		}
		switch c.Subject[len(c.Repo)] {
		case '#', '!':
			// the two canonical work-key separators worksource reserves
		default:
			return fmt.Errorf("task subject %q is not a canonical work key", c.Subject)
		}
	default:
		return fmt.Errorf("claim type %q is not in the accepted set {repo, task}", c.Type)
	}
	return nil
}

// Key returns the canonical claim key — "mutation:<repo>" for a repo claim,
// "mutation:<subject>" for a task claim — or "" for a claim that fails
// Validate, mirroring the worksource/outcome contract that "" is never a key.
func (c Claim) Key() string {
	if err := c.Validate(); err != nil {
		return ""
	}
	if c.Type == ClaimTypeRepo {
		return claimKeyPrefix + c.Repo
	}
	return claimKeyPrefix + c.Subject
}

// Overlaps applies the entire accepted overlap algebra: equal keys conflict;
// a repo claim conflicts with every claim in the same canonical repository;
// everything else is disjoint. No path math, no glob.
func (c Claim) Overlaps(other Claim) bool {
	ck, ok := c.Key(), other.Key()
	if ck == "" || ok == "" {
		// An unidentifiable claim overlaps nothing and can never be held.
		return false
	}
	if ck == ok {
		return true
	}
	if c.Repo != other.Repo {
		return false
	}
	return c.Type == ClaimTypeRepo || other.Type == ClaimTypeRepo
}

// RepoClaim mints the repository-wide/top claim for a canonical repo.
func RepoClaim(repo string) Claim { return Claim{Type: ClaimTypeRepo, Repo: repo} }

// TaskClaim mints the task claim for a canonical repo and work key.
func TaskClaim(repo, subjectKey string) Claim {
	return Claim{Type: ClaimTypeTask, Repo: repo, Subject: subjectKey}
}

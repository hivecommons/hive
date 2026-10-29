package dashboard

import (
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/claims"
)

// StageTrackerClaimant names who holds a tracker row right now — the
// identity, not just the kind the actor icon already draws. The SPA
// appends it to the lit chevron (" · quality", " · @alice",
// " · @bob's clanker", " · quality@alchemy") and adds one "Held by:" line
// to the chevron hover (hivecommons/hive#15422).
//
// Kind is claimantKindHiveAgent (Agent is the lane name; Hive is set only
// when the claim came from another hive), claimantKindClanker (Login is
// the contributor the connected hive acts for) or claimantKindPerson
// (Login is the GitHub login). Since is when they took it when known;
// Source says where the identity was read from.
type StageTrackerClaimant struct {
	Kind   string     `json:"kind"`
	Login  string     `json:"login,omitempty"`
	Agent  string     `json:"agent,omitempty"`
	Hive   string     `json:"hive,omitempty"`
	Since  *time.Time `json:"since,omitempty"`
	Source string     `json:"source"`
}

const (
	claimantKindHiveAgent = "hive-agent"
	claimantKindClanker   = "clanker"
	claimantKindPerson    = "person"

	claimantSourceClaimLedger  = "claim-ledger"
	claimantSourceClaimComment = "claim-comment"
	claimantSourceAssignee     = "assignee"
	claimantSourceKickHistory  = "kick-history"
	claimantSourceAuthor       = "author"

	agentLaneLabelPrefix = "agent/"
)

// stageTrackerClaimInputs is everything the claimant derivation reads,
// gathered by the tracker build so the derivation itself is pure.
type stageTrackerClaimInputs struct {
	// Ledger is the worker-claim ledger's live claim, when the ledger is
	// on and holds one.
	Ledger *claims.Claim
	// MarkerKind / MarkerHolder / MarkerHive are the issue's live claim as
	// read from GitHub (Issue.ClaimKind / ClaimedBy / ClaimHive): a
	// `hive:claim` marker when MarkerKind is set, else an assignee or lease.
	MarkerKind, MarkerHolder, MarkerHive string
	Assignees                            []string
	// Worker is the busy agent on the item from the status payload, when any.
	Worker     *stageTrackerWorker
	Since      *time.Time
	HiveID     string
	Labels     []string
	Author     string
	AuthorKind string
	// HiveAgent is the agent named by a hive PR's `— hive:` attribution
	// trailer (github.PullRequest.HiveAgent), when the enumeration kept it.
	HiveAgent string
	// IsClankerOwner reports whether a GitHub login belongs to a registered
	// contributor with a connected hive (a clanker owner). Nil means unknown.
	IsClankerOwner func(login string) bool
}

// stageTrackerIssueClaimant derives the holder of a claimed / in-progress
// issue: the worker ledger first, then the claim marker read from GitHub,
// then an assignee, then the agent the status payload says is on it. Nil
// when nothing names anyone.
func stageTrackerIssueClaimant(in stageTrackerClaimInputs) *StageTrackerClaimant {
	if c := in.Ledger; c != nil && c.Holder != "" {
		out := &StageTrackerClaimant{Source: claimantSourceClaimLedger, Since: in.Since}
		if !c.ClaimedAt.IsZero() {
			out.Since = timePtr(c.ClaimedAt)
		}
		switch c.Kind {
		case claims.KindAgent:
			out.Kind, out.Agent = claimantKindHiveAgent, c.Holder
		case claims.KindContributor, claims.KindExternal:
			out.Kind, out.Login = claimantKindClanker, c.Holder
		default:
			out.Kind, out.Login = claimantKindPerson, c.Holder
		}
		out.Hive = stageTrackerForeignHive(c.Hive, in.HiveID)
		return out
	}
	if in.MarkerHolder != "" {
		out := &StageTrackerClaimant{Source: claimantSourceClaimComment, Since: in.Since}
		switch in.MarkerKind {
		case string(claims.KindAgent):
			out.Kind, out.Agent = claimantKindHiveAgent, in.MarkerHolder
		case string(claims.KindContributor), string(claims.KindExternal):
			out.Kind, out.Login = claimantKindClanker, in.MarkerHolder
		case string(claims.KindHuman):
			out.Kind, out.Login = claimantKindPerson, in.MarkerHolder
		default:
			// No kind: the live claim is an assignee (issueclaim.SourceAssignee).
			out.Source = claimantSourceAssignee
			out.Kind, out.Login = stageTrackerLoginKind(in.MarkerHolder, in.IsClankerOwner), in.MarkerHolder
		}
		out.Hive = stageTrackerForeignHive(in.MarkerHive, in.HiveID)
		return out
	}
	for _, a := range in.Assignees {
		if a = strings.TrimSpace(a); a != "" {
			return &StageTrackerClaimant{Kind: stageTrackerLoginKind(a, in.IsClankerOwner), Login: a, Since: in.Since, Source: claimantSourceAssignee}
		}
	}
	if w := in.Worker; w != nil && w.Agent != "" {
		out := &StageTrackerClaimant{Kind: claimantKindHiveAgent, Agent: w.Agent, Since: in.Since, Source: claimantSourceKickHistory}
		if !w.At.IsZero() {
			out.Since = timePtr(w.At)
		}
		return out
	}
	return nil
}

// stageTrackerPRClaimant derives who holds a PR row: a live worker-ledger
// claim on the PR first (source claim-ledger), else the author, by the author kind the row
// already resolved. A hive PR names the agent on it (the status payload's
// busy agent, else the attribution trailer's agent, else the agent/<lane>
// label) and the adopting agent counts the same way. Nil for a foreign bot
// or an unknown author kind (hivecommons/hive#15422; PR in progress rows
// too, hivecommons/hive#15541).
func stageTrackerPRClaimant(in stageTrackerClaimInputs) *StageTrackerClaimant {
	if c := in.Ledger; c != nil && c.Holder != "" {
		return stageTrackerIssueClaimant(stageTrackerClaimInputs{Ledger: c, Since: in.Since, HiveID: in.HiveID})
	}
	switch in.AuthorKind {
	case authorKindHive:
		out := &StageTrackerClaimant{Kind: claimantKindHiveAgent, Since: in.Since, Source: claimantSourceAuthor}
		if w := in.Worker; w != nil && w.Agent != "" {
			out.Agent, out.Source = w.Agent, claimantSourceKickHistory
			if !w.At.IsZero() {
				out.Since = timePtr(w.At)
			}
			return out
		}
		out.Agent = strings.TrimSpace(in.HiveAgent)
		if out.Agent == "" {
			out.Agent = stageTrackerAgentLane(in.Labels)
		}
		if out.Agent == "" {
			out.Login = in.Author
		}
		if out.Agent == "" && out.Login == "" {
			return nil
		}
		return out
	case authorKindClanker:
		if in.Author == "" {
			return nil
		}
		return &StageTrackerClaimant{Kind: claimantKindClanker, Login: in.Author, Since: in.Since, Source: claimantSourceAuthor}
	case authorKindHuman:
		if in.Author == "" {
			return nil
		}
		return &StageTrackerClaimant{Kind: claimantKindPerson, Login: in.Author, Since: in.Since, Source: claimantSourceAuthor}
	}
	return nil
}

// stageTrackerAgentLane is the lane named by an agent/<lane> label, or "".
func stageTrackerAgentLane(labels []string) string {
	for _, l := range labels {
		if lane, ok := strings.CutPrefix(strings.TrimSpace(l), agentLaneLabelPrefix); ok && lane != "" {
			return lane
		}
	}
	return ""
}

// stageTrackerLoginKind is clanker for a registered contributor's login
// (its connected hive acts for them), else person.
func stageTrackerLoginKind(login string, isClankerOwner func(string) bool) string {
	if isClankerOwner != nil && isClankerOwner(login) {
		return claimantKindClanker
	}
	return claimantKindPerson
}

// stageTrackerForeignHive is the claim's hive when it is not this hive.
func stageTrackerForeignHive(hive, local string) string {
	hive = strings.TrimSpace(hive)
	if hive == "" || strings.EqualFold(hive, strings.TrimSpace(local)) {
		return ""
	}
	return hive
}

// stageTrackerClaimantStage says whether a row at this stage shows its
// holder: issues while claimed or with a PR under way; PRs from the moment
// they exist (an unclassified or in-progress PR lights "PR in progress"
// under its issue, hivecommons/hive#15541) through review, the author's
// rework and adoption, before the merge gate (where the gate word carries
// its own live detail).
func stageTrackerClaimantStage(kind string, stage int) bool {
	if kind == "pr" {
		return stage < trackerNumberAwaitingMerge
	}
	return stage == trackerNumberClaimed || stage == trackerNumberPRInProgress
}

// stageTrackerIsClankerOwner memoises the contributor-registry lookup for
// one tracker build: a login with a profile is a clanker owner.
func stageTrackerIsClankerOwner() func(string) bool {
	seen := map[string]bool{}
	return func(login string) bool {
		key := strings.ToLower(strings.TrimSpace(login))
		if key == "" {
			return false
		}
		if v, ok := seen[key]; ok {
			return v
		}
		v := findContributor(strings.TrimSpace(login)) != nil
		seen[key] = v
		return v
	}
}

// stageTrackerLedgerClaim reads the worker ledger's live claim for the
// issue, nil when the ledger is off or holds none.
func (s *Server) stageTrackerLedgerClaim(repo string, number int) *claims.Claim {
	l := s.claimsLedger()
	if l == nil {
		return nil
	}
	c, ok := l.Lookup(repo, number)
	if !ok {
		return nil
	}
	return &c
}

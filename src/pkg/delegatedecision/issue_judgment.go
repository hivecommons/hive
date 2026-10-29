package delegatedecision

import (
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Owner-style judgment for parked issues (hivecommons/hive: "Let hive decide"
// should decide the way the owner would). Three questions come before the
// reversible default:
//
//   - a fleet self-report whose remedy is an operator's (credentials, quota,
//     network, config on the reporting spoke) is not code work: the hive tells
//     the spoke's owner what to do and keeps the item out of the actionable set;
//   - a hive-filed proposal is approved when it carries evidence — an incident
//     it would have prevented, or a recurrence it would end;
//   - a hive-filed proposal without such evidence is declined for now.
//
// Each classifier is pure so the table tests run on the real issue bodies.

// SelfReport is what the delegate reads from a `hive:selfreport` issue.
type SelfReport struct {
	// Class is the runtime fault class (for example agent_login_required).
	Class string
	// Hive is the reporting hive id from the marker; Spoke the spoke name from
	// the evidence table (falls back to Hive); SpokeURL its link when given.
	Hive, Spoke, SpokeURL string
	// Owners are the @logins the evidence table names as owner(s).
	Owners []string
}

var (
	selfreportMarkerRE = regexp.MustCompile(`<!-- hive:selfreport\b([^>]*)-->`)
	selfreportHiveRE   = regexp.MustCompile(`\bhive=([^\s>]+)`)
	selfreportClassRE  = regexp.MustCompile("(?i)\\bclass\\s*`([a-z0-9_]+)`")
	selfreportTitleRE  = regexp.MustCompile(`^\s*\[selfreport\]\s*([a-z0-9_]+)`)
	selfreportRowRE    = regexp.MustCompile(`(?m)^\|\s*([^|]+?)\s*\|\s*([^|]*?)\s*\|`)
	selfreportLinkRE   = regexp.MustCompile(`^\[([^\]]+)\]\(([^)]+)\)`)
	mentionRE          = regexp.MustCompile(`@([A-Za-z0-9](?:[A-Za-z0-9-]*[A-Za-z0-9])?)`)
)

// ParseSelfReport recognises a fleet self-report issue: the body carries the
// `hive:selfreport` marker or the title starts with `[selfreport]`. The class
// comes from the body ("class `x`") or the title prefix.
func ParseSelfReport(title, body string) (SelfReport, bool) {
	var sr SelfReport
	marker := selfreportMarkerRE.FindStringSubmatch(body)
	titleMatch := selfreportTitleRE.FindStringSubmatch(title)
	if marker == nil && titleMatch == nil {
		return sr, false
	}
	if marker != nil {
		if m := selfreportHiveRE.FindStringSubmatch(marker[1]); m != nil {
			sr.Hive = m[1]
		}
	}
	if m := selfreportClassRE.FindStringSubmatch(body); m != nil {
		sr.Class = strings.ToLower(m[1])
	} else if titleMatch != nil {
		sr.Class = strings.ToLower(titleMatch[1])
	}
	for _, row := range selfreportRowRE.FindAllStringSubmatch(body, -1) {
		key, val := strings.ToLower(strings.TrimSpace(row[1])), strings.TrimSpace(row[2])
		switch key {
		case "spoke":
			if link := selfreportLinkRE.FindStringSubmatch(val); link != nil {
				sr.Spoke, sr.SpokeURL = link[1], link[2]
			} else if val != "" && val != "—" {
				sr.Spoke = val
			}
		case "owner(s)", "owners", "owner":
			for _, m := range mentionRE.FindAllStringSubmatch(val, -1) {
				sr.Owners = appendUnique(sr.Owners, m[1])
			}
		case "hive":
			if sr.Hive == "" && val != "" && val != "—" {
				sr.Hive = val
			}
		}
	}
	if sr.Spoke == "" {
		sr.Spoke = sr.Hive
	}
	return sr, sr.Class != ""
}

// selfreportOpsRemedies maps the self-report classes whose remedy is an
// operator's — credentials, quota, disk, network or App configuration on the
// reporting spoke — to the one action its owner takes. Code-defect classes
// (panics, repeated unclassified errors, stalls) are absent on purpose: they
// stay on the implementer path. The names mirror pkg/fleetreport's class
// constants (pinned by TestSelfreportOpsClassesMatchFleetReport) without
// importing that package's dependency tree into this leaf.
var selfreportOpsRemedies = map[string]string{
	"agent_login_required":          "re-authenticate the agent provider (run the provider's login on the spoke so the agent CLI has a current session)",
	"inference_auth_failed":         "renew the inference credentials or API key the spoke's agents use",
	"data_disk_pressure":            "free or grow the data disk on the spoke",
	"github_app_token_mint_failed":  "check the GitHub App key and installation the spoke is configured with",
	"github_app_not_installed":      "install the GitHub App on the organisation or repositories the spoke serves",
	"github_app_repo_not_covered":   "add the repository to the GitHub App installation the spoke uses",
	"proxy_connect_dial_failed":     "restore the spoke's outbound proxy or network path",
	"proxy_tls_handshake_failed":    "fix the spoke's proxy TLS trust (certificate or CA bundle)",
	"crux_runtime_unavailable":      "restart or re-deploy the crux runtime sidecar on the spoke",
	"model_discovery_degraded":      "check the model endpoint and credentials the spoke's provider is configured with",
	"inference_upstream_error":      "check the inference endpoint the spoke's provider points at",
	"inference_caller_unidentified": "set the inference caller identity in the spoke's configuration",
	"label_provisioning_failed":     "grant the spoke's GitHub identity the label-admin permission it lacks",
	"shared_ci_rerun_forbidden":     "grant the spoke's GitHub identity the actions permission needed to re-run shared CI",
}

// selfreportOpsClass reports whether a self-report class is an operator's to
// fix and, if so, the remedy in one clause.
func selfreportOpsClass(class string) (remedy string, ok bool) {
	remedy, ok = selfreportOpsRemedies[strings.ToLower(strings.TrimSpace(class))]
	return remedy, ok
}

// OperatorInstruction is the one-clause direction a self-report's spoke
// owner receives: who, what, where.
func (sr SelfReport) OperatorInstruction(remedy string) string {
	who := "the spoke owner"
	if len(sr.Owners) > 0 {
		who = "@" + strings.Join(sr.Owners, " @")
	}
	spoke := sr.Spoke
	if spoke == "" {
		spoke = "the reporting spoke"
	}
	return who + ": " + remedy + " on " + spoke
}

// operatorActionPrefix opens an operator-action decision's text; the
// effect line strips it to read "Waiting on the spoke owner: @who: …".
const operatorActionPrefix = "operator action — "

// Proposal shapes: the hive's planning and quality lanes file proposals with
// a lane prefix or as a gate proposal / RFC.
var (
	proposalTitleRE = regexp.MustCompile(`(?i)\[(?:quality|scanner|architect|strategist|outreach)\]|\bgate proposal\b`)
	proposalRFCRE   = regexp.MustCompile(`\bRFC\b`)
	incidentRefRE   = regexp.MustCompile(`(?i)\bincident\s*#\d+`)
	// incidentRE is evidence the proposal would have prevented something that
	// happened: an incident reference, breakage or a measured blast radius.
	incidentRE = regexp.MustCompile(`(?i)\b(?:broke|regression|outage|uncompilable|went red|was red|blast radius)\b`)
	// recurrenceRE is evidence of a class the proposal would end: the prior
	// instances it names were each fixed one at a time.
	recurrenceRE = regexp.MustCompile(`(?i)\b(?:fix(?:ed|ing)?\s+(?:in|by)|fixing PRs?:)\s*((?:#\d+(?:\s*[,/]\s*|\s+and\s+|\s*)?)+)`)
	issueRefRE   = regexp.MustCompile(`#(\d+)`)
)

// Evidence kinds a proposal can carry.
const (
	EvidenceIncident   = "incident"
	EvidenceRecurrence = "recurrence"
)

// IsProposal reports a proposal-shaped issue: a lane-prefixed title
// ([quality], [scanner], [architect], [strategist], [outreach]), a "Gate
// proposal" or RFC, or a proposal/design label.
func IsProposal(title string, labels []string) bool {
	if hasAnyLabel(labels, labelKindProposal, labelProposal, labelRFC, labelKindDesign) {
		return true
	}
	return proposalTitleRE.MatchString(title) || proposalRFCRE.MatchString(title)
}

// ProposalEvidence reports whether a proposal shows the evidence the owner
// asks for before paying for a gate: an incident (kind EvidenceIncident) or a
// recurrence of fixed instances (kind EvidenceRecurrence). detail quotes the
// evidence in plain words for the rationale.
func ProposalEvidence(title, body string) (kind, detail string, ok bool) {
	text := title + "\n" + body
	if m := incidentRefRE.FindString(text); m != "" {
		return EvidenceIncident, "it cites " + strings.ToLower(m), true
	}
	if m := incidentRE.FindString(text); m != "" {
		return EvidenceIncident, "it describes an incident (" + strings.ToLower(m) + ")", true
	}
	fixed := map[int]bool{}
	for _, m := range recurrenceRE.FindAllStringSubmatch(text, -1) {
		for _, ref := range issueRefRE.FindAllStringSubmatch(m[1], -1) {
			if n, err := strconv.Atoi(ref[1]); err == nil {
				fixed[n] = true
			}
		}
	}
	if len(fixed) >= 2 {
		nums := make([]int, 0, len(fixed))
		for n := range fixed {
			nums = append(nums, n)
		}
		sort.Ints(nums)
		refs := make([]string, 0, len(nums))
		for _, n := range nums {
			refs = append(refs, "#"+strconv.Itoa(n))
		}
		return EvidenceRecurrence, "the same class was already fixed one at a time in " + strings.Join(refs, ", "), true
	}
	return "", "", false
}

// BotShapedLogin reports a login that is an App or bot rather than a person.
func BotShapedLogin(login string) bool {
	l := strings.ToLower(strings.TrimSpace(login))
	return strings.HasSuffix(l, "[bot]") || strings.HasPrefix(l, "app/")
}

// hiveFiled reports an issue the hive (or any bot) filed: the only authors
// whose proposals the hive declines on its own.
func hiveFiled(in Input) bool {
	return in.HiveAuthored || BotShapedLogin(in.Author)
}

func appendUnique(list []string, s string) []string {
	for _, have := range list {
		if strings.EqualFold(have, s) {
			return list
		}
	}
	return append(list, s)
}

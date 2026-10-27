package priorart

import (
	"fmt"
	"strings"
	"time"
)

// Decision is what the author or a maintainer replied to the gate.
type Decision int

const (
	// DecisionNone: not a gate reply (a plain comment still un-parks).
	DecisionNone Decision = iota
	// DecisionProceed: `/hive decision proceed — <what is missing>`.
	DecisionProceed
	// DecisionExists: `/hive decision exists` — close as already available.
	DecisionExists
)

// Reply is a parsed gate reply.
type Reply struct {
	Decision Decision
	// Delta is the text after "proceed" — what the existing work lacks.
	Delta string
}

// ParseReply reads the first `/hive decision …` line of a comment. Any
// separator between the verb and the delta is accepted: "—", "-", ":" or
// just whitespace. Unknown verbs are DecisionNone.
func ParseReply(body string) Reply {
	for _, line := range strings.Split(body, "\n") {
		l := strings.TrimSpace(line)
		low := strings.ToLower(l)
		if !strings.HasPrefix(low, "/hive decision") {
			continue
		}
		rest := strings.TrimSpace(l[len("/hive decision"):])
		verb := rest
		var tail string
		if i := strings.IndexAny(rest, " \t—–-:"); i >= 0 {
			verb, tail = rest[:i], rest[i:]
		}
		tail = strings.TrimLeft(tail, " \t—–-:")
		switch strings.ToLower(verb) {
		case "proceed", "continue", "go":
			return Reply{Decision: DecisionProceed, Delta: strings.TrimSpace(tail)}
		case "exists", "existing", "duplicate", "dupe", "already-exists":
			return Reply{Decision: DecisionExists}
		}
	}
	return Reply{}
}

// RenderParkComment is the gate's park comment: the marker, the ask, the
// findings and the two replies.
func RenderParkComment(r Report) string {
	var b strings.Builder
	b.WriteString(Marker)
	b.WriteString("\n**Possibly exists · needs author**\n\n")
	b.WriteString("Before this is worked, please review — these look like they already cover the request:\n\n")
	b.WriteString(renderFindingList(r))
	b.WriteString("\nReply `/hive decision proceed — <what is missing>` to continue, or `/hive decision exists` to close as already available.\n")
	b.WriteString("\n<sub>Prior-art gate (hivecommons/hive#13575). No agent claims this issue while it carries `stage/017-possible-existing`; a plain reply from the author or a maintainer also un-parks it.</sub>\n")
	return b.String()
}

// FindingsMarker opens the non-parking findings note attached to a
// hive-authored issue: the agent that filed it (or the one that claims it)
// must state the gap, but no person is asked to review the hive's own
// findings.
const FindingsMarker = "<!-- hive:prior-art-findings -->"

// RenderFindingsNote is the comment attached to a hive-authored issue that
// matched prior art at medium or high confidence. It does not park.
func RenderFindingsNote(r Report) string {
	var b strings.Builder
	b.WriteString(FindingsMarker)
	b.WriteString("\n**Prior art found — gap vs existing required**\n\n")
	b.WriteString("This issue was filed by a hive agent and looks close to work that already exists:\n\n")
	b.WriteString(renderFindingList(r))
	b.WriteString("\nThe agent that claims this issue must open its claim comment with a **Gap vs existing** paragraph naming what the items above lack; if nothing is missing, close this issue with `/hive decision exists`.\n")
	return b.String()
}

// RenderGapSection is the "Gap vs existing" section a hive agent appends to
// the body of an issue it files despite a medium-confidence match.
func RenderGapSection(r Report) string {
	var b strings.Builder
	b.WriteString("## Gap vs existing\n\n")
	b.WriteString("Prior art found before filing:\n\n")
	b.WriteString(renderFindingList(r))
	b.WriteString("\nWhat the above does not cover:\n\n- [ ] (state the gap)\n")
	return b.String()
}

func renderFindingList(r Report) string {
	var b strings.Builder
	for i, f := range r.Findings {
		fmt.Fprintf(&b, "%d. %s %s (%d%% match", i+1, KindLabel(f.Kind), linkOrRef(f), f.Confidence)
		if len(f.Matched) > 0 {
			fmt.Fprintf(&b, " on `%s`", strings.Join(f.Matched, "`, `"))
		}
		b.WriteString(")")
		if f.Quote != "" {
			fmt.Fprintf(&b, "\n   > %s", f.Quote)
		}
		b.WriteString("\n")
	}
	if len(r.Coverage) > 0 {
		fmt.Fprintf(&b, "\n<sub>Search coverage incomplete — %s. More prior art may exist.</sub>\n", mdEscape(strings.Join(r.Coverage, "; ")))
	}
	return b.String()
}

func linkOrRef(f Finding) string {
	title := strings.TrimSpace(f.Title)
	switch {
	case f.URL != "" && title != "":
		return fmt.Sprintf("[%s](%s)", mdEscape(title), f.URL)
	case f.URL != "":
		return fmt.Sprintf("[%s](%s)", mdEscape(firstNonEmpty(f.Ref, f.URL)), f.URL)
	case title != "" && f.Ref != "":
		return fmt.Sprintf("`%s` — %s", f.Ref, mdEscape(title))
	case title != "":
		return mdEscape(title)
	default:
		return "`" + f.Ref + "`"
	}
}

// KindLabel is the human label for a finding kind ("merged PR", "closed
// issue"), shared by the gate's comments and the relay's refusal messages.
func KindLabel(k Kind) string {
	switch k {
	case KindMergedPR:
		return "merged PR"
	case KindClosedIssue:
		return "closed issue"
	case KindOpenIssue:
		return "open issue"
	case KindDoc:
		return "doc"
	case KindSetting:
		return "setting"
	case KindKnowledge:
		return "knowledge"
	case KindCode:
		return "code"
	}
	return string(k)
}

func mdEscape(s string) string {
	return strings.NewReplacer("[", "\\[", "]", "\\]", "|", "\\|").Replace(s)
}

func firstNonEmpty(vs ...string) string {
	for _, v := range vs {
		if v != "" {
			return v
		}
	}
	return ""
}

// ScopeHeading opens the section AppendScopeSection adds to the issue body.
const ScopeHeading = "## Scope vs existing"

// AppendScopeSection records the author's `proceed` delta in the issue body
// as acceptance criteria, so the agent that later claims the issue builds
// the gap and not the thing that exists. Idempotent per gate comment URL.
func AppendScopeSection(body, delta, gateCommentURL, actor string) string {
	if gateCommentURL != "" && strings.Contains(body, gateCommentURL) && strings.Contains(body, ScopeHeading) {
		return body
	}
	delta = strings.TrimSpace(delta)
	var b strings.Builder
	b.WriteString(strings.TrimRight(body, "\n"))
	if b.Len() > 0 {
		b.WriteString("\n\n")
	}
	b.WriteString(ScopeHeading)
	b.WriteString("\n\n")
	if gateCommentURL != "" {
		fmt.Fprintf(&b, "The prior-art gate found work that may already cover this ([findings](%s)). ", gateCommentURL)
	} else {
		b.WriteString("The prior-art gate found work that may already cover this. ")
	}
	if actor != "" {
		fmt.Fprintf(&b, "@%s confirmed the gap:", actor)
	} else {
		b.WriteString("The author confirmed the gap:")
	}
	b.WriteString("\n\n")
	if delta == "" {
		b.WriteString("- [ ] (no delta given — the claiming agent must state in its first comment what the existing work lacks before building)\n")
	} else {
		for _, line := range strings.Split(delta, "\n") {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			line = strings.TrimLeft(line, "-*• ")
			line = strings.TrimPrefix(line, "[ ] ")
			fmt.Fprintf(&b, "- [ ] %s\n", line)
		}
	}
	return b.String()
}

// IsFeatureKind reports whether an issue is of a kind the gate judges:
// a label matching one of kinds (bare or `kind/`-prefixed, case-insensitive)
// or a leading `[RFC]` / `RFC:` / `Epic:` title tag naming a kind.
// Issues carrying a bug label are never feature kind, whatever else they
// carry.
func IsFeatureKind(title string, labels []string, kinds []string) bool {
	if len(kinds) == 0 {
		return false
	}
	want := map[string]bool{}
	for _, k := range kinds {
		want[strings.ToLower(strings.TrimSpace(k))] = true
	}
	for _, l := range labels {
		low := strings.ToLower(strings.TrimSpace(l))
		if low == "bug" || low == "kind/bug" || strings.HasPrefix(low, "bug/") || low == "regression" {
			return false
		}
	}
	for _, l := range labels {
		low := strings.ToLower(strings.TrimSpace(l))
		if want[low] {
			return true
		}
		if i := strings.Index(low, "/"); i >= 0 && want[low[i+1:]] {
			return true
		}
	}
	t := strings.TrimSpace(strings.ToLower(title))
	for _, tag := range titleTags(t) {
		if want[tag] {
			return true
		}
	}
	return false
}

// titleTags returns the leading tags of a title: "[rfc] x" → rfc,
// "epic: x" → epic, "rfc — x" → rfc.
func titleTags(t string) []string {
	var out []string
	for i := 0; i < 3; i++ {
		t = strings.TrimSpace(t)
		if strings.HasPrefix(t, "[") {
			j := strings.Index(t, "]")
			if j <= 0 || j > 24 {
				break
			}
			out = append(out, strings.TrimSpace(t[1:j]))
			t = t[j+1:]
			continue
		}
		j := strings.IndexAny(t, ":—–-")
		if j <= 0 || j > 16 {
			break
		}
		tag := strings.TrimSpace(t[:j])
		if strings.ContainsAny(tag, " /.") {
			break
		}
		out = append(out, tag)
		t = afterSep(t[j:])
	}
	return out
}

// RenderClankerParkComment is RenderParkComment for an issue a clanker
// (another hive, a contributor CLI, an external bot) filed: the same ask,
// plus notice that silence closes it after ttl. A zero ttl means no
// auto-close and renders exactly RenderParkComment.
func RenderClankerParkComment(r Report, ttl time.Duration) string {
	body := RenderParkComment(r)
	if ttl <= 0 {
		return body
	}
	return strings.TrimRight(body, "\n") + fmt.Sprintf("\n\n<sub>Filed by an automated author: with no reply in %s this is closed as `already-exists` (reopen with `/hive decision proceed — <what is missing>`).</sub>\n", formatTTL(ttl))
}

// AutoCloseMarker tags the reply the resume sweep leaves when it closes a
// clanker-filed park after the TTL.
const AutoCloseMarker = "<!-- hive:prior-art-autoclose -->"

func formatTTL(d time.Duration) string {
	if d%(24*time.Hour) == 0 {
		return fmt.Sprintf("%dd", int(d/(24*time.Hour)))
	}
	if d%time.Hour == 0 {
		return fmt.Sprintf("%dh", int(d/time.Hour))
	}
	return d.String()
}

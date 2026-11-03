package priorart

import "strings"

// AuthorKind is who filed the issue, which decides what the gate does with
// a match (hivecommons/hive#13575):
//
//   - a person is parked and asked; never auto-closed;
//   - the hive's own agent is never parked (there is nobody to ask): the
//     findings are attached and the issue carries on, and at filing time a
//     high-confidence match means "do not file";
//   - a clanker (another hive, a contributor CLI, an external bot) is parked
//     like a person and auto-closed after a TTL with no reply.
type AuthorKind string

const (
	AuthorHuman   AuthorKind = "human"
	AuthorHive    AuthorKind = "hive"
	AuthorClanker AuthorKind = "clanker"
)

// Authorship is the evidence ClassifyAuthor reads. It is a plain struct so
// the scheduler, the resume sweep and tests build it from whatever view of
// the issue they hold.
type Authorship struct {
	Login string
	// IsHuman is the enumerator's verdict (github.Issue.AuthorIsHuman): a
	// person other than this hive, not a bot.
	IsHuman bool
	// IsHiveAccount marks the hive's own App or agent accounts.
	IsHiveAccount bool
	Labels        []string
}

// ClassifyAuthor reads the `author/<kind>` label first (pkg/authorkind,
// hive beats clanker beats human), then the agent/<lane> and origin/<src>
// provenance families the hive puts on its own filings, then the login:
// a `[bot]` login or a non-human, non-hive account is a clanker.
func ClassifyAuthor(a Authorship) AuthorKind {
	var byLabel AuthorKind
	for _, l := range a.Labels {
		low := strings.ToLower(strings.TrimSpace(l))
		switch {
		case low == "author/hive":
			return AuthorHive
		case low == "author/clanker":
			byLabel = AuthorClanker
		case low == "author/human" && byLabel == "":
			byLabel = AuthorHuman
		}
	}
	if byLabel != "" {
		return byLabel
	}
	if a.IsHiveAccount {
		return AuthorHive
	}
	for _, l := range a.Labels {
		low := strings.ToLower(strings.TrimSpace(l))
		if strings.HasPrefix(low, "agent/") || strings.HasPrefix(low, "origin/") || low == "hive/selfreport" {
			return AuthorHive
		}
	}
	if a.IsHuman {
		return AuthorHuman
	}
	return AuthorClanker
}

// FilingVerdict is what a hive agent about to file an issue should do with
// a prior-art report.
type FilingVerdict int

const (
	// FileAsIs: nothing relevant was found; file normally.
	FileAsIs FilingVerdict = iota
	// FileWithFindings: a medium match — file, but attach the findings and
	// an explicit "gap vs existing" section.
	FileWithFindings
	// DoNotFile: a high-confidence match — do not file; record a knowledge
	// bead instead.
	DoNotFile
)

// MediumConfidenceFloor is how far under the park threshold a match still
// counts as "medium" for FilingVerdictFor.
const MediumConfidenceFloor = 20

// FilingVerdictFor applies the three-way filing rule for hive-authored
// issues: at or above threshold do not file; within MediumConfidenceFloor
// under it file with findings; otherwise file as is.
func FilingVerdictFor(r Report, threshold int) FilingVerdict {
	switch {
	case len(r.Findings) == 0:
		return FileAsIs
	case r.Confidence >= threshold:
		return DoNotFile
	case r.Confidence >= threshold-MediumConfidenceFloor:
		return FileWithFindings
	default:
		return FileAsIs
	}
}

// DiscoverabilityTitle is the knowledge bead title recorded when a person
// asked for something that already exists: the docs, not the person, are
// the gap (hivecommons/hive#13575).
func DiscoverabilityTitle(issueTitle string) string {
	t := strings.TrimSpace(stripTitleTags(issueTitle))
	if t == "" {
		t = "a feature"
	}
	return "discoverability: " + t + " was asked for though it exists"
}

// DiscoverabilityDetail is the bead body: the issue, the best finding and
// where it lives, so a docs pass can link the two.
func DiscoverabilityDetail(issueURL string, r Report) string {
	var b strings.Builder
	b.WriteString("Asked in ")
	b.WriteString(issueURL)
	b.WriteString(".")
	if len(r.Findings) > 0 {
		f := r.Findings[0]
		b.WriteString(" Already covered by ")
		b.WriteString(KindLabel(f.Kind))
		b.WriteString(" ")
		b.WriteString(firstNonEmpty(f.URL, f.Ref, f.Title))
		if f.Quote != "" {
			b.WriteString(" — \"")
			b.WriteString(f.Quote)
			b.WriteString("\"")
		}
		b.WriteString(".")
	}
	b.WriteString(" Consider linking the existing feature from the README, docs or CHANGELOG where the request would have looked for it.")
	return b.String()
}

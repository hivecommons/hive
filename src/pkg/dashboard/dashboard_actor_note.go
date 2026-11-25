package dashboard

import (
	"context"
	"regexp"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/config"
)

// Dashboard actor notes (hivecommons/hive#19852). A label the dashboard
// adds or removes for a person is written with the Hive App's token, so the
// issue's GitHub timeline names the App as the actor: a maintainer's own
// "Release hold" read as automation overriding a human hold. The audit log
// names the person, but only on the hive. So each such action also leaves
// one comment on the item naming the signed-in dashboard user, kept under a
// hidden marker per kind and edited in place on the next action of that
// kind instead of repeated. dashboard.attribute_label_actions turns it off.

const (
	dashboardActorKindHold     = "hold"
	dashboardActorKindTriage   = "triage"
	dashboardActorKindApproval = "approval"

	// dashboardActorNoteMaxEntries bounds the entries one note keeps,
	// newest first.
	dashboardActorNoteMaxEntries = 10
	// dashboardActorNoteTimeout bounds the comment write so a slow GitHub
	// does not hold the action's response.
	dashboardActorNoteTimeout = 10 * time.Second
	dashboardActorNoteHeader  = "The Hive App changed labels on this item for a person who acted from the Hive dashboard, so GitHub shows the App as the actor. Who asked (newest first):"
)

// dashboardActorLoginRe is a GitHub login: alphanumerics and hyphens, not
// leading with a hyphen, at most 39 characters. Anything else (a hub
// identity such as `ibmid:…`) is named without an @-mention.
var dashboardActorLoginRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]{0,38}$`)

func dashboardActorNoteMarker(kind string) string {
	return "<!-- hive:dashboard-actor kind=" + kind + " -->"
}

// dashboardActorName is how a note names the dashboard user: an @-mention
// for a GitHub login, the identity in code for anything else, and the hive
// owner for an owner session that carries no user.
func dashboardActorName(user string) string {
	user = strings.TrimSpace(user)
	switch {
	case user == "":
		return "the hive owner"
	case dashboardActorLoginRe.MatchString(user):
		return "@" + user
	default:
		return "`" + strings.ReplaceAll(user, "`", "") + "`"
	}
}

// dashboardActorEntry is one line of a note: what was done, by whom, what
// labels changed (may be empty) and when.
func dashboardActorEntry(action, user, detail string, at time.Time) string {
	entry := action + " from the Hive dashboard by " + dashboardActorName(user)
	if detail = strings.TrimSpace(detail); detail != "" {
		entry += ": " + detail
	}
	return entry + " (" + at.UTC().Format("2006-01-02 15:04") + " UTC)."
}

// dashboardActorLabelChange words a label change: "`a`, `b` added;
// `c` removed", either half omitted when empty.
func dashboardActorLabelChange(added, removed []string) string {
	var parts []string
	if len(added) > 0 {
		parts = append(parts, "`"+strings.Join(added, "`, `")+"` added")
	}
	if len(removed) > 0 {
		parts = append(parts, "`"+strings.Join(removed, "`, `")+"` removed")
	}
	return strings.Join(parts, "; ")
}

// renderDashboardActorNote is the note body after entry: the marker, the
// header, entry and then the entries previous already listed, capped at
// dashboardActorNoteMaxEntries.
func renderDashboardActorNote(marker, previous, entry string) string {
	lines := []string{"- " + entry}
	for _, line := range strings.Split(previous, "\n") {
		if len(lines) >= dashboardActorNoteMaxEntries {
			break
		}
		if line = strings.TrimSpace(line); strings.HasPrefix(line, "- ") {
			lines = append(lines, line)
		}
	}
	return marker + "\n" + dashboardActorNoteHeader + "\n\n" + strings.Join(lines, "\n")
}

// dashboardActorNotesEnabled reads dashboard.attribute_label_actions.
func (s *Server) dashboardActorNotesEnabled() bool {
	if s == nil || s.deps == nil || s.deps.GHClient == nil {
		return false
	}
	return s.deps.Config == nil || s.deps.Config.Dashboard.LabelActionAttributionEnabled()
}

// noteDashboardActor records entry on repoFull#number's note of kind. The
// label change it describes has already landed, so a failure is logged and
// never fails the action; the audit log entry stands either way. A paused
// repository takes no comment.
func (s *Server) noteDashboardActor(ctx context.Context, repoFull string, number int, kind, entry string) {
	if !s.dashboardActorNotesEnabled() {
		return
	}
	if _, paused := s.repoWritesPaused(repoFull); paused {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, dashboardActorNoteTimeout)
	defer cancel()
	marker := dashboardActorNoteMarker(kind)
	_, err := s.deps.GHClient.EditOperatorMarkerComment(ctx, repoFull, number, marker, func(previous string) string {
		return renderDashboardActorNote(marker, previous, entry)
	})
	if err != nil && s.logger != nil {
		s.logger.Warn("dashboard actor note not posted; the audit log still names the user", "repo", repoFull, "number", number, "kind", kind, "error", err)
	}
}

// noteApprovalReleased records the labels a dashboard approval took off a
// PR for user (released as repoItemApprovalReleased names them): the
// `needs-human` park and the sentinel label. The relayed `/lgtm` and
// `/approve` comments already name the person; the label removals did not.
func (s *Server) noteApprovalReleased(ctx context.Context, repoFull string, number int, user string, released []string) {
	var removed []string
	for _, r := range released {
		switch r {
		case "needs-human":
			removed = append(removed, config.DefaultNeedsHumanLabel)
		case "sentinel-label":
			removed = append(removed, repoItemSentinelLabelOrDefault(s.sentinelLabel()))
		}
	}
	if len(removed) == 0 {
		return
	}
	s.noteDashboardActor(ctx, repoFull, number, dashboardActorKindApproval,
		dashboardActorEntry("Approved", user, dashboardActorLabelChange(nil, removed), time.Now()))
}

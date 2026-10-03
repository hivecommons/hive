// Package holdguard closes the hold-gate branch-hygiene gap (#5589): a PR
// sitting hold-gated can silently accumulate commits from OTHER authors or
// agents (a worktree cut from a contaminated base, a helpful agent pushing to
// someone else's branch), and nothing re-reviewed that drift before the hold
// lifted and the merge lanes re-opened. The human gate only works if the diff
// the human approved is the diff that merges.
//
// The guard is deterministic hub-side machinery, parallel to (and deliberately
// independent of) the fix-loop escalation ledger in pkg/escalation — it never
// touches escalation.Entry semantics, so the formally verified escalation
// invariants are unaffected. Mechanics:
//
//  1. While a PR is held, the governor snapshots its head SHA plus the commit
//     SHA / author sets into this ledger (first-held snapshot wins — that is
//     the tree the hold gate showed the human).
//  2. When the hold lifts, the snapshot is compared against the current head.
//     Head unchanged ⇒ the author set cannot have changed either (the head SHA
//     pins the entire history) — the entry clears and merge lanes reopen.
//  3. Head moved (new commits, force-push, rebase) ⇒ the PR is kept out of
//     merge eligibility, a one-time evidence comment names the new commits and
//     authors (plain text, never @-mentions), the hold label is re-applied so
//     EVERY merge lane re-gates, and the snapshot re-arms at the drifted head.
//     A human lifting the re-applied hold after reading the evidence IS the
//     fresh approval — that lift compares clean and the PR proceeds.
//
// Held-gated PRs must also stay mergeable, not just frozen (#10437): while a
// PR is held, the hive may merge its base in with its own identity to clear a
// "behind"/"dirty" mergeable state. That is hygiene, not reviewable content,
// so AdvanceHygiene moves the ledger's baseline to the resulting head instead
// of letting step 2/3 read it as drift on the next lift. When the hive cannot
// resolve a real conflict this way, a one-time comment (ConflictCommentBody)
// tells the human before they open the review, instead of letting them
// discover a stale, unmergeable PR mid-review.
package holdguard

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// ReHoldLabel is the label the guard re-applies to a drifted PR. It MUST be a
// member of github.HoldLabels so the re-applied label actually re-engages the
// hold machinery (enumeration moves the PR back into Hold.Items and both
// auto-merge sweeps skip it); cmd/hive pins that agreement with a test.
const ReHoldLabel = "hold"

// Retention is how long an entry survives without being observed (held, lifted
// clean, or drifted). Entries for PRs that merged or closed while held have no
// clearing event, so age is the only safe reaper. Fourteen days matches the
// governor's audit PR-attribution window; deliberately NOT a prune-on-absence
// sweep like pkg/escalation uses, because forgetting a snapshot after one
// failed repo enumeration would let the very next tick re-snapshot a
// contaminated head as if a human had approved it.
const Retention = 14 * 24 * time.Hour

// maxTrackedCommits bounds the per-PR snapshot so a pathological branch cannot
// grow the ledger without limit. Matches GitHub's PR commit-list ceiling.
const maxTrackedCommits = 250

// maxListedCommits bounds how many new commits the drift comment enumerates
// line-by-line; the remainder is summarized as a count.
const maxListedCommits = 10

// shortSHALen is the abbreviated commit-SHA length used in the drift comment.
const shortSHALen = 12

// Commit is one commit on a hold-gated PR's branch: enough identity for the
// snapshot sets and the drift comment, nothing more.
type Commit struct {
	SHA string `json:"sha"`
	// Author is the forge login when known, otherwise the git author name.
	Author string `json:"author,omitempty"`
	// Title is the first line of the commit message.
	Title string `json:"title,omitempty"`
}

// Entry is the persisted per-PR snapshot taken while the PR sits hold-gated.
type Entry struct {
	// HeadSHA is the branch head observed when the PR was (last) snapshotted —
	// the tree the hold gate showed the human. An unchanged head at lift time
	// proves the entire history is unchanged.
	HeadSHA string `json:"head_sha"`
	// CommitSHAs are the branch's commit SHAs at snapshot time (bounded by
	// maxTrackedCommits). Empty when the commit list was unavailable; the
	// drift diff then fails toward treating every current commit as new.
	CommitSHAs []string `json:"commit_shas,omitempty"`
	// Authors is the sorted, de-duplicated author set at snapshot time.
	Authors []string `json:"authors,omitempty"`
	// Commented is set once the drift comment posted for the CURRENT drift
	// episode, so a retried label write never re-comments. ReArm resets it —
	// a later, separate drift is a new episode and earns fresh evidence.
	Commented bool `json:"commented,omitempty"`
	// ConflictNoted is set once the "can't keep this mergeable" comment
	// posted for the CURRENT conflict episode (#10437), mirroring Commented's
	// dedup role for drift. AdvanceHygiene resets it the moment a
	// merge-from-base succeeds — the conflict that earned the notice is
	// gone, so a stale "can't merge" comment must not linger unexplained.
	ConflictNoted bool `json:"conflict_noted,omitempty"`
	// HeldAt is when the snapshot was first recorded.
	HeldAt    time.Time `json:"held_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Drift is the verdict comparing a lift-time head against the hold snapshot.
type Drift struct {
	// Moved reports that the branch is not provably the reviewed tree. It is
	// true whenever the heads differ AND whenever either head is unknown —
	// this guard fails closed.
	Moved           bool
	RecordedHeadSHA string
	CurrentHeadSHA  string
	// NewCommits are current commits absent from the snapshot's SHA set. A
	// rebase/force-push renames every SHA, so after one, every commit is
	// (correctly) new: the reviewed tree no longer exists.
	NewCommits []Commit
	// NewAuthors are authors absent from the snapshot's author set.
	NewAuthors []string
}

// Store is the on-PVC hold-snapshot ledger. All methods are safe for
// concurrent use. Persistence conventions mirror pkg/escalation: a JSON map
// keyed "repo#number", written atomically via tmp+rename.
type Store struct {
	mu      sync.Mutex
	path    string
	entries map[string]*Entry
	// clock is the time source; nil means time.Now().UTC(). Overridable via
	// SetClock so retention tests are deterministic.
	clock func() time.Time
}

// Load reads the ledger at path, returning an empty usable store on any error
// (a missing or corrupt ledger must never block enumeration).
func Load(path string) *Store {
	s := &Store{path: path, entries: map[string]*Entry{}}
	data, err := os.ReadFile(path)
	if err != nil {
		return s
	}
	var m map[string]*Entry
	if json.Unmarshal(data, &m) == nil && m != nil {
		s.entries = m
	}
	return s
}

// Key builds the ledger key for a PR.
func Key(repo string, number int) string {
	return fmt.Sprintf("%s#%d", repo, number)
}

// Snapshot records the hold-time snapshot for a PR, but only if none exists:
// the FIRST held observation is the tree the human gate saw, and later pushes
// while still held must not quietly become the baseline. Returns whether a
// new snapshot was recorded. An empty headSHA records nothing — there is no
// tree to pin.
func (s *Store) Snapshot(repo string, number int, headSHA string, commits []Commit) bool {
	if headSHA == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	key := Key(repo, number)
	if s.entries[key] != nil {
		return false
	}
	now := s.now()
	e := &Entry{HeadSHA: headSHA, HeldAt: now, UpdatedAt: now}
	e.CommitSHAs, e.Authors = commitSets(commits)
	s.entries[key] = e
	s.saveLocked()
	return true
}

// Touch refreshes an entry's UpdatedAt so a long-standing hold is never aged
// out by Prune while the PR is still visibly held. No-op for unknown PRs.
func (s *Store) Touch(repo string, number int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e := s.entries[Key(repo, number)]; e != nil {
		e.UpdatedAt = s.now()
		s.saveLocked()
	}
}

// Recorded returns a copy of the PR's snapshot entry, if one exists.
func (s *Store) Recorded(repo string, number int) (Entry, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.entries[Key(repo, number)]
	if e == nil {
		return Entry{}, false
	}
	out := *e
	out.CommitSHAs = append([]string(nil), e.CommitSHAs...)
	out.Authors = append([]string(nil), e.Authors...)
	return out, true
}

// Clear removes the PR's snapshot — the hold lifted with the reviewed tree
// intact (or the drift episode fully resolved).
func (s *Store) Clear(repo string, number int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.entries[Key(repo, number)]; !ok {
		return
	}
	delete(s.entries, Key(repo, number))
	s.saveLocked()
}

// MarkCommented records that the drift comment posted for the current drift
// episode, so a retried hold-label write never duplicates the evidence.
func (s *Store) MarkCommented(repo string, number int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e := s.entries[Key(repo, number)]; e != nil {
		e.Commented = true
		e.UpdatedAt = s.now()
		s.saveLocked()
	}
}

// ReArm replaces the PR's snapshot with the drifted head — called ONLY after
// the hold label was successfully re-applied, so the human lifting that hold
// is approving exactly this tree. Commented resets: any later drift is a new
// episode and earns its own evidence comment.
func (s *Store) ReArm(repo string, number int, headSHA string, commits []Commit) {
	if headSHA == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	key := Key(repo, number)
	now := s.now()
	e := s.entries[key]
	if e == nil {
		e = &Entry{HeldAt: now}
		s.entries[key] = e
	}
	e.HeadSHA = headSHA
	e.CommitSHAs, e.Authors = commitSets(commits)
	e.Commented = false
	e.UpdatedAt = now
	s.saveLocked()
}

// AdvanceHygiene moves a held PR's baseline forward to a head the hive itself
// produced by merging the base into the held branch to keep it mergeable
// (#10437). Unlike ReArm, this is NOT a response to drift: the hive's own
// merge-from-base is hygiene, not reviewable content, so the moved head must
// never register as drift on the PR's next lift check — advancing the
// baseline here is what keeps Diff from flagging it later. No-op for a PR the
// guard never snapshotted (nothing to advance) or an empty headSHA (nothing
// to advance to). ConflictNoted resets: the branch merges cleanly again, so a
// stale "can't keep this mergeable" comment would no longer be true.
func (s *Store) AdvanceHygiene(repo string, number int, headSHA string, commits []Commit) {
	if headSHA == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	key := Key(repo, number)
	e := s.entries[key]
	if e == nil {
		return
	}
	e.HeadSHA = headSHA
	e.CommitSHAs, e.Authors = commitSets(commits)
	e.ConflictNoted = false
	e.UpdatedAt = s.now()
	s.saveLocked()
}

// MarkConflictNoted records that the "can't keep this mergeable" comment
// posted for the CURRENT conflict episode, so a retried attempt never
// duplicates the notice. AdvanceHygiene clears it once the conflict resolves.
func (s *Store) MarkConflictNoted(repo string, number int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e := s.entries[Key(repo, number)]; e != nil {
		e.ConflictNoted = true
		e.UpdatedAt = s.now()
		s.saveLocked()
	}
}

// Prune drops entries not observed within retention. See Retention for why
// this is age-based rather than prune-on-absence.
func (s *Store) Prune(retention time.Duration) {
	if retention <= 0 {
		retention = Retention
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	cutoff := s.now().Add(-retention)
	changed := false
	for key, e := range s.entries {
		if e.UpdatedAt.Before(cutoff) {
			delete(s.entries, key)
			changed = true
		}
	}
	if changed {
		s.saveLocked()
	}
}

// SetClock overrides the store's time source. Intended for tests so retention
// can be exercised deterministically without sleeping.
func (s *Store) SetClock(fn func() time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.clock = fn
}

func (s *Store) now() time.Time {
	if s.clock != nil {
		return s.clock()
	}
	return time.Now().UTC()
}

func (s *Store) saveLocked() {
	if s.path == "" {
		return
	}
	data, err := json.MarshalIndent(s.entries, "", "  ")
	if err != nil {
		return
	}
	_ = os.MkdirAll(filepath.Dir(s.path), 0o755)
	tmp := s.path + ".tmp"
	if os.WriteFile(tmp, data, 0o644) == nil {
		_ = os.Rename(tmp, s.path)
	}
}

// commitSets flattens a commit list into the snapshot's bounded SHA list and
// sorted, de-duplicated author set.
func commitSets(commits []Commit) (shas, authors []string) {
	if len(commits) > maxTrackedCommits {
		commits = commits[len(commits)-maxTrackedCommits:]
	}
	seenAuthor := map[string]bool{}
	for _, c := range commits {
		if c.SHA != "" {
			shas = append(shas, c.SHA)
		}
		if c.Author != "" && !seenAuthor[c.Author] {
			seenAuthor[c.Author] = true
			authors = append(authors, c.Author)
		}
	}
	sort.Strings(authors)
	return shas, authors
}

// Diff compares a lift-time observation against the hold snapshot. It fails
// closed: an unknown recorded or current head reads as Moved. When the
// snapshot carried no commit list (the fetch failed at hold time), every
// current commit is treated as new — more evidence, never less.
func Diff(recorded Entry, currentHeadSHA string, current []Commit) Drift {
	d := Drift{
		RecordedHeadSHA: recorded.HeadSHA,
		CurrentHeadSHA:  currentHeadSHA,
	}
	if recorded.HeadSHA == "" || currentHeadSHA == "" || recorded.HeadSHA != currentHeadSHA {
		d.Moved = true
	}
	if !d.Moved {
		return d
	}
	known := map[string]bool{}
	for _, sha := range recorded.CommitSHAs {
		known[sha] = true
	}
	knownAuthor := map[string]bool{}
	for _, a := range recorded.Authors {
		knownAuthor[a] = true
	}
	seenNewAuthor := map[string]bool{}
	for _, c := range current {
		if c.SHA != "" && !known[c.SHA] {
			d.NewCommits = append(d.NewCommits, c)
			if c.Author != "" && !knownAuthor[c.Author] && !seenNewAuthor[c.Author] {
				seenNewAuthor[c.Author] = true
				d.NewAuthors = append(d.NewAuthors, c.Author)
			}
		}
	}
	sort.Strings(d.NewAuthors)
	return d
}

// shortSHA abbreviates a commit SHA for the drift comment.
func shortSHA(sha string) string {
	if len(sha) > shortSHALen {
		return sha[:shortSHALen]
	}
	return sha
}

// CommentBody renders the one-time drift-evidence comment. Author logins are
// rendered in backticks, NEVER as @-mentions — the point is evidence for
// whoever reviews the PR, not a notification blast at whoever's commits got
// smuggled in.
func CommentBody(d Drift) string {
	var b strings.Builder
	b.WriteString("## 🔒 Hold-gate integrity: branch changed while hold-gated — fresh review required\n\n")
	fmt.Fprintf(&b,
		"This PR's branch moved while it sat hold-gated: the head recorded when the hold was applied was `%s`, but the branch now sits at `%s`. ",
		shortSHA(d.RecordedHeadSHA), shortSHA(d.CurrentHeadSHA))
	b.WriteString("The diff a reviewer saw under the hold is no longer the diff that would merge, so auto-merge is blocked and the `" + ReHoldLabel + "` label has been re-applied.\n\n")

	if len(d.NewCommits) > 0 {
		fmt.Fprintf(&b, "**Commits not present in the hold-time snapshot (%d):**\n\n", len(d.NewCommits))
		listed := d.NewCommits
		if len(listed) > maxListedCommits {
			listed = listed[:maxListedCommits]
		}
		for _, c := range listed {
			line := "- `" + shortSHA(c.SHA) + "`"
			if c.Author != "" {
				line += " by `" + c.Author + "`"
			}
			if c.Title != "" {
				line += " — " + c.Title
			}
			b.WriteString(line + "\n")
		}
		if extra := len(d.NewCommits) - len(listed); extra > 0 {
			fmt.Fprintf(&b, "- …and %d more\n", extra)
		}
		b.WriteString("\n")
	} else {
		b.WriteString("The current commit list could not be compared against the snapshot; treat the entire branch as unreviewed.\n\n")
	}

	if len(d.NewAuthors) > 0 {
		quoted := make([]string, len(d.NewAuthors))
		for i, a := range d.NewAuthors {
			quoted[i] = "`" + a + "`"
		}
		fmt.Fprintf(&b, "**Authors not in the hold-time author set:** %s\n\n", strings.Join(quoted, ", "))
	}

	b.WriteString("A human should review the FULL diff at the current head — a rebase or force-push renames every commit, so everything above needs eyes even if it looks familiar. ")
	b.WriteString("Removing the hold label after that review is the fresh approval: the guard has re-pinned its snapshot to the current head, so a clean lift re-opens the merge lanes.\n")
	return b.String()
}

// ConflictCommentBody renders the one-time notice posted when a held PR falls
// behind its base with a real conflict the hive cannot resolve by merging the
// base in (#10437). Held PRs otherwise stay quietly out of date while waiting
// on a human — this tells the human up front that the review they are about
// to do is of a PR that cannot merge as-is, instead of letting them discover
// a DIRTY PR mid-review.
func ConflictCommentBody() string {
	var b strings.Builder
	b.WriteString("## ⚠️ Hold-gate mergeability: this PR has a real conflict with its base\n\n")
	b.WriteString("This PR is hold-gated and has fallen behind its base branch. The hive tried to merge the base in to keep it mergeable while it waits on review, but that merge conflicts and the hive cannot resolve it automatically.\n\n")
	b.WriteString("It will stay held and will NOT merge on its own even once reviewed — removing `" + ReHoldLabel + "` alone will not be enough. Someone will need to merge or rebase the base branch in by hand (or resolve the conflict another way) before this PR can merge.\n")
	return b.String()
}

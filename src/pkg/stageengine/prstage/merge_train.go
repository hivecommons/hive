package prstage

import (
	"path"
	"strconv"
	"strings"
)

// Merge-train reasons (hivecommons/hive#15117). The automerge sweep groups
// candidate PRs on the same base whose changed files overlap, merges one
// front at a time and parks the rest behind it. The park is recorded as a
// lane reason so the stage engine (080 merge gate, never a human stage) and
// the dashboard (the "In merge train · 3rd of 4" chevron word) read the
// same fact. Wire format:
//
//	merge-train:<state>#<front>:<position>/<size>:<attempt>/<max>:<file>,<file>[,+N]
//
// where <state> is one of the MergeTrainState* constants, <front> is the
// PR number the train merges first, <position> is this PR's 1-based slot,
// <attempt>/<max> counts the rebase attempts the train has spent on this
// PR behind this front (0/0 when none applies) and the files are the base
// names of the overlapping paths (at most MergeTrainReasonMaxFiles, the
// rest counted as "+N").
const (
	MergeTrainReasonPrefix = "merge-train:"
	// MergeTrainStateQueuedBehind: a sibling waiting for the front to merge.
	MergeTrainStateQueuedBehind = "queued-behind"
	// MergeTrainStateRebasing: the train's front is conflicting and the
	// hive has asked its rebase-resolve lane to rebase it onto the base.
	MergeTrainStateRebasing = "rebasing"
	// MergeTrainStateRebaseExhausted: the front stayed conflicting after
	// the configured number of rebase attempts; a person decides (085).
	MergeTrainStateRebaseExhausted = "rebase-exhausted"
	// MergeTrainStateFront is never written by the sweep: the dashboard
	// synthesises it on the PR the siblings name as their front.
	MergeTrainStateFront = "front"
	// MergeTrainStateDuplicateOf: the PR declares the same `Fixes #N` as
	// another open PR the train admitted; it is held out of the train
	// behind that PR (Front) until a person or the delegate picks one
	// (hivecommons/hive#15395). Position / Size carry the issue number
	// as Position and 0 as Size. The stage engine parks it with its
	// author (070/071/072 by author kind), never on the merge gate, so
	// the tracker reads "returned" with the gate pill "Duplicate of #N"
	// rather than "Approved; waiting its turn" (hivecommons/hive#15469).
	MergeTrainStateDuplicateOf = "duplicate-of"
	// MergeTrainStateClaimConflict: two or more open PRs claim the same
	// issue and no decision has been recorded on this one, so every lane
	// refuses to admit it (hivecommons/hive#15810). Front is the other
	// claimant, Position the issue. Unlike DuplicateOf the queue picked
	// nobody: the PR parks at 085 until a person or the delegate decides.
	MergeTrainStateClaimConflict = "claim-conflict"
	MergeTrainReasonMaxFiles     = 4
)

// MergeTrain is the decoded form of a merge-train reason.
type MergeTrain struct {
	State    string `json:"state"`
	Front    int    `json:"front"`
	Position int    `json:"position"`
	Size     int    `json:"size"`
	// Attempt / MaxAttempts count the rebases the train has asked for on
	// this PR behind this front; both 0 when the PR is merely queued.
	Attempt      int      `json:"attempt,omitempty"`
	MaxAttempts  int      `json:"max_attempts,omitempty"`
	OverlapFiles []string `json:"overlap_files,omitempty"`
}

// Reason encodes the train position as a lane reason.
func (m MergeTrain) Reason() string {
	var b strings.Builder
	b.WriteString(MergeTrainReasonPrefix)
	b.WriteString(m.State)
	b.WriteByte('#')
	b.WriteString(strconv.Itoa(m.Front))
	b.WriteByte(':')
	b.WriteString(strconv.Itoa(m.Position))
	b.WriteByte('/')
	b.WriteString(strconv.Itoa(m.Size))
	b.WriteByte(':')
	b.WriteString(strconv.Itoa(m.Attempt))
	b.WriteByte('/')
	b.WriteString(strconv.Itoa(m.MaxAttempts))
	b.WriteByte(':')
	b.WriteString(strings.Join(mergeTrainFileNames(m.OverlapFiles), ","))
	return b.String()
}

// mergeTrainFileNames shortens overlapping paths to base names, capped at
// MergeTrainReasonMaxFiles with a "+N" tail, de-duplicated and in order.
func mergeTrainFileNames(files []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(files))
	for _, f := range files {
		name := path.Base(strings.TrimSpace(f))
		if name == "" || name == "." || seen[name] {
			continue
		}
		seen[name] = true
		out = append(out, name)
	}
	if len(out) > MergeTrainReasonMaxFiles {
		rest := len(out) - MergeTrainReasonMaxFiles
		out = append(out[:MergeTrainReasonMaxFiles:MergeTrainReasonMaxFiles], "+"+strconv.Itoa(rest))
	}
	return out
}

// ParseMergeTrain decodes a merge-train reason (with or without the
// `:human-approved@login` suffix). ok is false for any other reason.
func ParseMergeTrain(reason string) (m MergeTrain, ok bool) {
	rest, found := strings.CutPrefix(strings.TrimSpace(reason), MergeTrainReasonPrefix)
	if !found {
		return MergeTrain{}, false
	}
	if i := strings.Index(rest, ":human-approved@"); i >= 0 {
		rest = rest[:i]
	}
	state, rest, found := strings.Cut(rest, "#")
	if !found || state == "" {
		return MergeTrain{}, false
	}
	front, rest, found := strings.Cut(rest, ":")
	if !found {
		return MergeTrain{}, false
	}
	slot, rest, found := strings.Cut(rest, ":")
	if !found {
		return MergeTrain{}, false
	}
	pos, size, found := strings.Cut(slot, "/")
	if !found {
		return MergeTrain{}, false
	}
	tries, files, _ := strings.Cut(rest, ":")
	attempt, maxAttempts, found := strings.Cut(tries, "/")
	if !found {
		return MergeTrain{}, false
	}
	var err error
	if m.Front, err = strconv.Atoi(front); err != nil {
		return MergeTrain{}, false
	}
	if m.Position, err = strconv.Atoi(pos); err != nil {
		return MergeTrain{}, false
	}
	if m.Size, err = strconv.Atoi(size); err != nil {
		return MergeTrain{}, false
	}
	if m.Attempt, err = strconv.Atoi(attempt); err != nil {
		return MergeTrain{}, false
	}
	if m.MaxAttempts, err = strconv.Atoi(maxAttempts); err != nil {
		return MergeTrain{}, false
	}
	m.State = state
	for _, f := range strings.Split(files, ",") {
		if f = strings.TrimSpace(f); f != "" {
			m.OverlapFiles = append(m.OverlapFiles, f)
		}
	}
	return m, true
}

// ClaimConflictCaption is the chevron caption of a PR held on an undecided
// same-issue claim conflict: "held · duplicate of #B · needs decision".
func ClaimConflictCaption(other int) string {
	return "held · duplicate of #" + strconv.Itoa(other) + " · needs decision"
}

// IsMergeTrainReason reports whether reason carries a merge-train position.
func IsMergeTrainReason(reason string) bool {
	return strings.HasPrefix(strings.TrimSpace(reason), MergeTrainReasonPrefix)
}

// Ordinal renders a 1-based position as "1st", "2nd", "3rd", "4th", ….
func Ordinal(n int) string {
	suffix := "th"
	switch {
	case n%100 >= 11 && n%100 <= 13:
	case n%10 == 1:
		suffix = "st"
	case n%10 == 2:
		suffix = "nd"
	case n%10 == 3:
		suffix = "rd"
	}
	return strconv.Itoa(n) + suffix
}

// Sentence is the one-line explanation the dashboard gate cell and the
// sweep log share: "behind #15074 in merge train (3rd of 4, overlapping
// files: stage_tracker.go, index.html)".
func (m MergeTrain) Sentence() string {
	files := ""
	if len(m.OverlapFiles) > 0 {
		files = ", overlapping files: " + strings.Join(m.OverlapFiles, ", ")
	}
	slot := Ordinal(m.Position) + " of " + strconv.Itoa(m.Size)
	switch m.State {
	case MergeTrainStateRebasing:
		return "1st in the merge queue, conflicting; the hive is rebasing it onto the base (attempt " + strconv.Itoa(m.Attempt) + "/" + strconv.Itoa(m.MaxAttempts) + "; " + slot + files + ")"
	case MergeTrainStateRebaseExhausted:
		return "1st in the merge queue, still conflicting after " + strconv.Itoa(m.MaxAttempts) + " rebase attempts; a person decides (" + slot + files + ")"
	case MergeTrainStateFront:
		return "1st in the merge queue; merges next (" + slot + files + ")"
	case MergeTrainStateDuplicateOf:
		issue := ""
		if m.Position > 0 {
			issue = " for issue #" + strconv.Itoa(m.Position)
		}
		return "duplicate of #" + strconv.Itoa(m.Front) + issue + "; superseded by #" + strconv.Itoa(m.Front) + " — the merge queue carries one PR per issue; close it or let the delegate decide"
	case MergeTrainStateClaimConflict:
		return ClaimConflictCaption(m.Front) + " (both claim issue #" + strconv.Itoa(m.Position) + "; the merge queue admits neither until one is closed or approved)"
	}
	return "behind #" + strconv.Itoa(m.Front) + " in merge train (" + slot + files + ")"
}

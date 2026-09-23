package publish

import (
	"fmt"
	"sort"
	"strings"
)

// Finding states the campaign records; only StateValidated is publishable.
const (
	StateValidated   = "validated"
	StateRejected    = "rejected"
	StateDuplicateOf = "duplicate_of"
)

// MarkerPrefix opens the HTML comment every published issue carries so the
// publisher can find its own issue again by finding hash: after a crash
// between intent and acknowledgment, and before any retry.
const MarkerPrefix = "<!-- hive-finding: "

// markerSuffix closes the marker comment.
const markerSuffix = " -->"

// RunTrailer is the trailer key on every published body naming the campaign
// run the finding came from.
const RunTrailer = "Hive-Run"

// Finding is the validated campaign finding handed to the publisher together
// with its inspection receipt. It carries evidence, never authority: the
// publisher re-checks policy and identity before anything is filed.
type Finding struct {
	// Campaign is the campaign key the finding belongs to.
	Campaign string
	// Repo is the canonical owner/name the finding is about and the repo a
	// public issue would be filed in.
	Repo string
	// BeadID is the finding bead recorded by the campaign.
	BeadID string
	// Title is the finding title; it becomes the issue title.
	Title string
	// Evidence is the finding body: what was inspected and why it is a finding.
	Evidence string
	// Files are the files the finding cites.
	Files []string
	// ContentHash is the finding identity (#8318): the hash the campaign
	// derived from the component content the finding was validated against.
	ContentHash string
	// Predicate is the proof predicate that validated the finding.
	Predicate string
	// Labels are the labels the campaign attached; the classifier reads them.
	Labels []string
	// ReceiptDigest is the inspection StageReceipt digest that proves the
	// finding was recorded before publication was attempted.
	ReceiptDigest string
	// State is the campaign's finding state; only StateValidated publishes.
	State string
	// DuplicateOf names the earlier finding bead when State is duplicate_of.
	DuplicateOf string
	// RunKey identifies the campaign run for the Hive-Run trailer.
	RunKey string
	// RunURL links the published body back to the run.
	RunURL string
}

// Validate reports why a finding cannot be published: publication needs the
// finding identity, the receipt that proves it was recorded, and a
// canonical repository. A finding that is not validated is refused here so
// rejected and duplicate findings can never reach the forge.
func (f Finding) Validate() error {
	if strings.TrimSpace(f.Campaign) == "" {
		return fmt.Errorf("%w: campaign is required", ErrInvalidFinding)
	}
	if f.Repo == "" || strings.Count(f.Repo, "/") != 1 || strings.ContainsAny(f.Repo, "@#! \t") {
		return fmt.Errorf("%w: repo %q is not a canonical owner/name spelling", ErrInvalidFinding, f.Repo)
	}
	if strings.TrimSpace(f.BeadID) == "" {
		return fmt.Errorf("%w: finding bead id is required", ErrInvalidFinding)
	}
	if strings.TrimSpace(f.Title) == "" {
		return fmt.Errorf("%w: title is required", ErrInvalidFinding)
	}
	if strings.TrimSpace(f.ContentHash) == "" || strings.ContainsAny(f.ContentHash, "| \t\n") {
		return fmt.Errorf("%w: content hash %q is not a finding identity", ErrInvalidFinding, f.ContentHash)
	}
	if strings.TrimSpace(f.ReceiptDigest) == "" {
		return fmt.Errorf("%w: inspection receipt digest is required", ErrInvalidFinding)
	}
	if f.State != StateValidated {
		return fmt.Errorf("%w: finding state %q is not %s", ErrInvalidFinding, f.State, StateValidated)
	}
	return nil
}

// Marker is the exact HTML comment the published body carries for this
// finding, and the text the marker lookup searches for.
func (f Finding) Marker() string {
	return MarkerPrefix + f.ContentHash + markerSuffix
}

// Body renders the published body: the evidence, the cited files, the
// receipt digest, the link back to the run, the marker, and the Hive-Run
// trailer. The same rendering goes to a public issue and to a private repo
// channel; the notify channel deliberately does not use it.
func (f Finding) Body() string {
	var b strings.Builder
	b.WriteString(strings.TrimSpace(f.Evidence))
	b.WriteString("\n")
	if len(f.Files) > 0 {
		files := append([]string(nil), f.Files...)
		sort.Strings(files)
		b.WriteString("\nFiles:\n")
		for _, file := range files {
			b.WriteString("- `" + file + "`\n")
		}
	}
	b.WriteString("\nInspection receipt: `" + f.ReceiptDigest + "`\n")
	if f.RunURL != "" {
		b.WriteString("Run: " + f.RunURL + "\n")
	}
	b.WriteString("\n" + f.Marker() + "\n")
	if f.RunKey != "" {
		b.WriteString("\n" + RunTrailer + ": " + f.RunKey + "\n")
	}
	return b.String()
}

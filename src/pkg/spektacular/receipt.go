package spektacular

import (
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/convergence/proof"
	"github.com/hivecommons/hive/pkg/effects"
	"github.com/hivecommons/hive/pkg/outputschema"
)

// Receipt constants.
const (
	// ContractRevision names the status contract the receipt was produced under.
	ContractRevision = "spektacular-status/v1"
	// EngineName identifies Spektacular as the producing engine on the receipt.
	EngineName = "spektacular"
	// engineVersion is the CLI version the runner assumes; the status verb does
	// not report one, so it names the Spektacular PR that shipped the contract.
	engineVersion = "spektacular-pr-45"
)

// BuildReceipt produces the stage receipt Hive records when an artifact
// reaches final. Every field is derived from the status document and the
// lease, never from a file.
func BuildReceipt(st Stage, status ArtifactStatus, now time.Time) outputschema.StageReceipt {
	workKey := st.RunKey
	if st.Repo != "" {
		workKey = st.Repo + "!" + st.RunKey
	}
	started := status.CreatedAt
	if started.IsZero() {
		started = now
	}
	// closed_at is the frontmatter close date; when the artifact carries none
	// the advance instant is the end. updated_at is deliberately not a
	// fallback: it is a file mtime whenever no workflow state matches the
	// artifact, so it says nothing about when the stage finished.
	ended := status.ClosedAt
	if ended.IsZero() {
		ended = now
	}
	repo := st.Repo
	if repo == "" {
		repo = st.RunKey
	}
	steps := append([]string(nil), status.CompletedSteps...)
	artifacts := []outputschema.Artifact{{
		Repo:        repo,
		Path:        status.Kind + "/" + status.Name,
		Description: "Spektacular " + status.Kind + " reached document_status final",
	}}
	// OutputDigest must equal the validator's artifact digest (outputschema
	// stageReceiptArtifactDigest): repo, path, description per artifact, sorted.
	parts := make([]string, 0, len(artifacts))
	for _, artifact := range artifacts {
		parts = append(parts, strings.Join([]string{artifact.Repo, artifact.Path, artifact.Description}, "\x00"))
	}
	sort.Strings(parts)
	// InputRevision is artifact@<hash>: the hash pins the facts the advance
	// was decided on (kind, name, document_status, current_step,
	// completed_steps, closed_at). updated_at is excluded on purpose: a
	// checkout or reformat moves it without anything having happened, and
	// two observations of the same final document must hash the same.
	inputHash := effects.StableDigest(
		status.Kind, status.Name, string(status.DocumentStatus), status.CurrentStep,
		strings.Join(steps, "\x00"), status.ClosedAt.UTC().Format(time.RFC3339Nano),
	)
	return outputschema.StageReceipt{
		SchemaVersion:    outputschema.StageReceiptSchemaVersion,
		WorkKey:          workKey,
		AssignmentID:     st.TaskID,
		Generation:       st.Gen,
		Stage:            st.Stage,
		ContractRevision: ContractRevision,
		ExecutionKey:     effects.StableDigest(st.RunKey, st.Stage, strconv.FormatUint(st.Gen, 10)),
		Engine:           &outputschema.StageReceiptEngine{Name: EngineName, Version: engineVersion},
		InputRevision:    "artifact@" + inputHash,
		OutputDigest:     effects.StableDigest(parts...),
		ResultClass:      outputschema.ReceiptResultCompleted,
		StartedAt:        started.UTC().Format(time.RFC3339Nano),
		EndedAt:          ended.UTC().Format(time.RFC3339Nano),
		Provenance:       &proof.Provenance{Query: "spektacular " + status.Kind + " status " + status.Name},
		Artifacts:        artifacts,
	}
}

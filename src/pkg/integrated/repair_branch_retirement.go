package integrated

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/kubestellar/hive/pkg/automation"
	hivegithub "github.com/kubestellar/hive/pkg/github"
	"github.com/kubestellar/hive/pkg/visualhive"
)

const (
	repairRetirementSchema       = "hive.repair-retirement-outbox.v1"
	repairRetirementFile         = "repair-branch-retirements.json"
	repairRetirementPurposeMerge = "merged_repair"
	repairRetirementPurposeOldPR = "superseded_repair"
	repairRetirementClosePR      = "close_pr"
	repairRetirementDeleteBranch = "delete_branch"
	maxRepairRetirements         = 100
	maxRepairRetirementFileBytes = 512 * 1024
	maxRepairRetirementError     = 4096
)

var (
	repairRetirementBranch = regexp.MustCompile(`^hive/repair-[A-Za-z0-9._/-]{1,180}$`)
	repairFingerprint      = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,256}$`)
)

// RepairBranchRetirement is an immutable remote-resource binding plus a small
// durable phase machine. The seal detects partial or accidental local edits;
// repository identity and lifecycle bindings are revalidated before every
// remote mutation.
type RepairBranchRetirement struct {
	ID                    string    `json:"id"`
	Repository            string    `json:"repository"`
	RepositoryID          string    `json:"repository_id"`
	RepositoryFingerprint string    `json:"repository_fingerprint"`
	PRNumber              int       `json:"pr_number"`
	RetainedPRNumber      int       `json:"retained_pr_number,omitempty"`
	Branch                string    `json:"branch"`
	HeadSHA               string    `json:"head_sha"`
	MergeSHA              string    `json:"merge_sha,omitempty"`
	Purpose               string    `json:"purpose"`
	Phase                 string    `json:"phase"`
	Agent                 string    `json:"agent"`
	RepairAttempts        int       `json:"repair_attempts"`
	Attempts              int       `json:"attempts,omitempty"`
	LastError             string    `json:"last_error,omitempty"`
	CreatedAt             time.Time `json:"created_at"`
	LastAttemptAt         time.Time `json:"last_attempt_at,omitempty"`
	Seal                  string    `json:"seal"`
}

type RepairBranchRetirementState struct {
	SchemaVersion      string                   `json:"schema_version"`
	MigrationCompleted bool                     `json:"legacy_migration_completed"`
	Entries            []RepairBranchRetirement `json:"entries"`
	UpdatedAt          time.Time                `json:"updated_at"`
}

func (s *Store) LoadRepairBranchRetirements() (RepairBranchRetirementState, bool, error) {
	path := filepath.Join(s.dir, repairRetirementFile)
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return RepairBranchRetirementState{}, false, nil
	}
	if err != nil {
		return RepairBranchRetirementState{}, false, err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() <= 0 || info.Size() > maxRepairRetirementFileBytes {
		return RepairBranchRetirementState{}, false, fmt.Errorf("repair retirement outbox is not a bounded ordinary state file")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return RepairBranchRetirementState{}, false, err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var state RepairBranchRetirementState
	if err := decoder.Decode(&state); err != nil {
		return RepairBranchRetirementState{}, false, fmt.Errorf("decode repair retirement outbox: %w", err)
	}
	if err := requireJSONEOF(decoder); err != nil {
		return RepairBranchRetirementState{}, false, fmt.Errorf("decode repair retirement outbox: %w", err)
	}
	if err := validateRepairRetirementState(state); err != nil {
		return RepairBranchRetirementState{}, false, err
	}
	return state, true, nil
}

func requireJSONEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return fmt.Errorf("multiple JSON values are not allowed")
		}
		return err
	}
	return nil
}

func (s *Store) SaveRepairBranchRetirements(state RepairBranchRetirementState) error {
	state.SchemaVersion = repairRetirementSchema
	state.UpdatedAt = time.Now().UTC()
	sort.Slice(state.Entries, func(i, j int) bool { return state.Entries[i].ID < state.Entries[j].ID })
	for index := range state.Entries {
		seal, err := repairRetirementSeal(state.Entries[index])
		if err != nil {
			return err
		}
		state.Entries[index].Seal = seal
	}
	if err := validateRepairRetirementState(state); err != nil {
		return err
	}
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	if len(data)+1 > maxRepairRetirementFileBytes {
		return fmt.Errorf("repair retirement outbox exceeds %d bytes", maxRepairRetirementFileBytes)
	}
	return writeDurableStateFile(filepath.Join(s.dir, repairRetirementFile), append(data, '\n'))
}

func validateRepairRetirementState(state RepairBranchRetirementState) error {
	if state.SchemaVersion != repairRetirementSchema || state.UpdatedAt.IsZero() || len(state.Entries) > maxRepairRetirements {
		return fmt.Errorf("repair retirement outbox is incomplete or exceeds its %d-entry bound", maxRepairRetirements)
	}
	seen := map[string]bool{}
	resources := map[string]bool{}
	previousID := ""
	for index, entry := range state.Entries {
		if err := validateRepairRetirement(entry); err != nil {
			return err
		}
		if seen[entry.ID] {
			return fmt.Errorf("repair retirement outbox contains duplicate id %s", entry.ID)
		}
		seen[entry.ID] = true
		if index > 0 && entry.ID <= previousID {
			return fmt.Errorf("repair retirement outbox entries are not in canonical ID order")
		}
		previousID = entry.ID
		resource := strings.ToLower(entry.Repository + "\x00" + entry.Branch + "\x00" + entry.HeadSHA)
		if resources[resource] {
			return fmt.Errorf("repair retirement outbox binds one branch more than once")
		}
		resources[resource] = true
	}
	return nil
}

func validateRepairRetirement(entry RepairBranchRetirement) error {
	expectedID, err := repairRetirementID(entry)
	if err != nil {
		return err
	}
	expectedSeal, err := repairRetirementSeal(entry)
	if err != nil {
		return err
	}
	repositoryOwner, repositoryName, hasSlash := strings.Cut(entry.Repository, "/")
	repositoryID, repositoryIDErr := strconv.ParseInt(entry.RepositoryID, 10, 64)
	validPhase := entry.Phase == repairRetirementDeleteBranch || (entry.Purpose == repairRetirementPurposeOldPR && entry.Phase == repairRetirementClosePR)
	validPurpose := entry.Purpose == repairRetirementPurposeMerge || entry.Purpose == repairRetirementPurposeOldPR
	validAgent := entry.Agent == "quality" || entry.Agent == "sec-check" || entry.Agent == "ci-maintainer"
	if entry.ID != expectedID || entry.Seal != expectedSeal || !hasSlash || repositoryOwner == "" || repositoryName == "" ||
		entry.Repository != strings.TrimSpace(entry.Repository) || repositoryIDErr != nil || repositoryID <= 0 ||
		!repairFingerprint.MatchString(entry.RepositoryFingerprint) || entry.PRNumber <= 0 ||
		!repairRetirementBranch.MatchString(entry.Branch) || strings.Contains(entry.Branch, "..") || strings.Contains(entry.Branch, "//") ||
		!immutableCommit.MatchString(strings.ToLower(entry.HeadSHA)) || !validPurpose || !validPhase || !validAgent ||
		entry.RepairAttempts < 0 || entry.RepairAttempts > 100000 || entry.Attempts < 0 || entry.Attempts > 100000 ||
		len(entry.LastError) > maxRepairRetirementError || entry.CreatedAt.IsZero() || (!entry.LastAttemptAt.IsZero() && entry.LastAttemptAt.Before(entry.CreatedAt)) {
		return fmt.Errorf("repair retirement %q is incomplete, invalid, or not sealed", entry.ID)
	}
	if entry.Purpose == repairRetirementPurposeMerge {
		if entry.RetainedPRNumber != 0 || entry.Phase != repairRetirementDeleteBranch || !immutableCommit.MatchString(strings.ToLower(entry.MergeSHA)) {
			return fmt.Errorf("merged repair retirement %s has an invalid merge binding", entry.ID)
		}
	} else if entry.RetainedPRNumber <= 0 || entry.RetainedPRNumber == entry.PRNumber || entry.MergeSHA != "" {
		return fmt.Errorf("superseded repair retirement %s has an invalid retained pull request binding", entry.ID)
	}
	return nil
}

func repairRetirementID(entry RepairBranchRetirement) (string, error) {
	descriptor := struct {
		Repository, RepositoryID, RepositoryFingerprint string
		PRNumber, RetainedPRNumber                      int
		Branch, HeadSHA, MergeSHA, Purpose              string
	}{entry.Repository, entry.RepositoryID, entry.RepositoryFingerprint, entry.PRNumber, entry.RetainedPRNumber, entry.Branch, strings.ToLower(entry.HeadSHA), strings.ToLower(entry.MergeSHA), entry.Purpose}
	data, err := json.Marshal(descriptor)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:]), nil
}

func repairRetirementSeal(entry RepairBranchRetirement) (string, error) {
	entry.Seal = ""
	data, err := json.Marshal(entry)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:]), nil
}

func newRepairRetirement(config Config, finding visualhive.FindingLifecycle, prNumber int, retainedPRNumber int, branch, headSHA, mergeSHA, purpose, phase string) (RepairBranchRetirement, error) {
	entry := RepairBranchRetirement{
		Repository: config.Repository, RepositoryID: config.RepositoryID, RepositoryFingerprint: finding.RepositoryFingerprint,
		PRNumber: prNumber, RetainedPRNumber: retainedPRNumber, Branch: branch, HeadSHA: strings.ToLower(headSHA),
		MergeSHA: strings.ToLower(mergeSHA), Purpose: purpose, Phase: phase, Agent: repairActor(finding.OwningAgentHint),
		RepairAttempts: finding.RepairAttempts, CreatedAt: time.Now().UTC(),
	}
	entry.ID, _ = repairRetirementID(entry)
	entry.Seal, _ = repairRetirementSeal(entry)
	if err := validateRepairRetirement(entry); err != nil {
		return RepairBranchRetirement{}, err
	}
	return entry, nil
}

func addRepairRetirement(state *RepairBranchRetirementState, entry RepairBranchRetirement) (bool, error) {
	for _, existing := range state.Entries {
		if existing.ID == entry.ID {
			return false, nil
		}
		if strings.EqualFold(existing.Repository, entry.Repository) && existing.Branch == entry.Branch && strings.EqualFold(existing.HeadSHA, entry.HeadSHA) {
			return false, fmt.Errorf("repair branch %s at %s already has a different durable retirement binding", entry.Branch, entry.HeadSHA)
		}
	}
	if len(state.Entries) >= maxRepairRetirements {
		return false, fmt.Errorf("repair retirement outbox reached its %d-entry bound", maxRepairRetirements)
	}
	state.Entries = append(state.Entries, entry)
	return true, nil
}

func enqueueMergedRepairRetirement(store *Store, config Config, finding visualhive.FindingLifecycle, mergeSHA string) error {
	entry, err := newRepairRetirement(config, finding, finding.PRNumber, 0, finding.Branch, finding.RepairCommitSHA, mergeSHA, repairRetirementPurposeMerge, repairRetirementDeleteBranch)
	if err != nil {
		return fmt.Errorf("prepare merged repair branch retirement: %w", err)
	}
	state, exists, err := store.LoadRepairBranchRetirements()
	if err != nil {
		return err
	}
	if !exists {
		state = RepairBranchRetirementState{SchemaVersion: repairRetirementSchema, UpdatedAt: time.Now().UTC()}
	}
	added, err := addRepairRetirement(&state, entry)
	if err != nil || !added {
		return err
	}
	if err := store.AuditStrict(AuditEntry{Action: "enqueue_repair_branch_retirement", Allowed: true, Repository: config.Repository, Detail: repairRetirementDetail(entry)}); err != nil {
		return err
	}
	return store.SaveRepairBranchRetirements(state)
}

func enqueueSupersededRepairRetirement(store *Store, config Config, finding visualhive.FindingLifecycle, prNumber int, branch, headSHA string) error {
	entry, err := newRepairRetirement(config, finding, prNumber, finding.PRNumber, branch, headSHA, "", repairRetirementPurposeOldPR, repairRetirementClosePR)
	if err != nil {
		return fmt.Errorf("prepare superseded repair retirement: %w", err)
	}
	state, exists, err := store.LoadRepairBranchRetirements()
	if err != nil {
		return err
	}
	if !exists {
		state = RepairBranchRetirementState{SchemaVersion: repairRetirementSchema, UpdatedAt: time.Now().UTC()}
	}
	added, err := addRepairRetirement(&state, entry)
	if err != nil || !added {
		return err
	}
	if err := store.AuditStrict(AuditEntry{Action: "enqueue_repair_branch_retirement", Allowed: true, Repository: config.Repository, Detail: repairRetirementDetail(entry)}); err != nil {
		return err
	}
	return store.SaveRepairBranchRetirements(state)
}

// prepareRepairRetirements performs a one-time bounded migration for terminal
// findings written by versions that treated cleanup as best-effort. Active
// merged findings are checked every run to close the local MarkMerged/enqueue
// crash window.
func prepareRepairRetirements(store *Store, config Config, lifecycle *visualhive.LifecycleStore) error {
	state, exists, err := store.LoadRepairBranchRetirements()
	if err != nil {
		return err
	}
	if !exists {
		state = RepairBranchRetirementState{SchemaVersion: repairRetirementSchema, UpdatedAt: time.Now().UTC()}
	}
	snapshot := lifecycle.Snapshot()
	keys := make([]string, 0, len(snapshot.Findings))
	for key := range snapshot.Findings {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	legacyCount := 0
	changed := false
	for _, key := range keys {
		finding := snapshot.Findings[key]
		if finding == nil || finding.MergeSHA == "" || finding.Branch == "" || finding.RepairCommitSHA == "" || finding.PRNumber <= 0 {
			continue
		}
		active := finding.Status == visualhive.StatusMerged || finding.Status == visualhive.StatusPostMergeVerifying
		terminal := finding.Status == visualhive.StatusResolved || finding.Status == visualhive.StatusIssueClosed || finding.Status == visualhive.StatusCancelled
		if !active && (!terminal || state.MigrationCompleted) {
			continue
		}
		if terminal {
			legacyCount++
			if legacyCount > maxRepairRetirements {
				return fmt.Errorf("legacy repair retirement migration exceeds its %d-finding bound", maxRepairRetirements)
			}
		}
		entry, entryErr := newRepairRetirement(config, *finding, finding.PRNumber, 0, finding.Branch, finding.RepairCommitSHA, finding.MergeSHA, repairRetirementPurposeMerge, repairRetirementDeleteBranch)
		if entryErr != nil {
			return fmt.Errorf("bind repair retirement for finding %s: %w", key, entryErr)
		}
		added, addErr := addRepairRetirement(&state, entry)
		if addErr != nil {
			return addErr
		}
		changed = changed || added
	}
	if !state.MigrationCompleted {
		state.MigrationCompleted = true
		changed = true
	}
	if !changed && exists {
		return nil
	}
	if err := store.AuditStrict(AuditEntry{Action: "prepare_repair_branch_retirements", Allowed: true, Repository: config.Repository, Detail: fmt.Sprintf("legacy=%d pending=%d migration_completed=true", legacyCount, len(state.Entries))}); err != nil {
		return err
	}
	return store.SaveRepairBranchRetirements(state)
}

func reconcileRepairRetirements(ctx context.Context, store *Store, config Config, lifecycle *visualhive.LifecycleStore, client *hivegithub.Client, policy automation.Policy, uninstall bool) error {
	state, exists, err := store.LoadRepairBranchRetirements()
	if err != nil || !exists {
		return err
	}
	for _, entry := range state.Entries {
		if err := validateRepairRetirementBinding(config, lifecycle, entry); err != nil {
			return err
		}
	}
	for len(state.Entries) > 0 {
		entry := &state.Entries[0]
		if entry.Phase == repairRetirementClosePR {
			if err := authorizeRepairRetirement(store, lifecycle, config, policy, *entry, automation.ActionCloseRepairPR, uninstall); err != nil {
				return rememberRepairRetirementFailure(store, &state, 0, err)
			}
			marker := fmt.Sprintf("<!-- hive-repair: %s -->", entry.RepositoryFingerprint)
			if err := client.CloseRepairPullRequestExact(ctx, entry.Repository, entry.PRNumber, marker, entry.Branch, entry.HeadSHA); err != nil {
				return rememberRepairRetirementFailure(store, &state, 0, fmt.Errorf("close exact superseded repair PR #%d: %w", entry.PRNumber, err))
			}
			if err := store.AuditStrict(AuditEntry{Action: "repair_retirement_pr_closed", Allowed: true, Repository: entry.Repository, Detail: repairRetirementDetail(*entry)}); err != nil {
				return err
			}
			entry.Phase, entry.LastError = repairRetirementDeleteBranch, ""
			entry.Attempts++
			entry.LastAttemptAt = time.Now().UTC()
			if err := store.SaveRepairBranchRetirements(state); err != nil {
				return err
			}
		}
		if err := authorizeRepairRetirement(store, lifecycle, config, policy, *entry, automation.ActionDeleteRepairBranch, uninstall); err != nil {
			return rememberRepairRetirementFailure(store, &state, 0, err)
		}
		if err := client.DeleteRepairBranchExact(ctx, entry.Repository, entry.Branch, entry.HeadSHA); err != nil {
			return rememberRepairRetirementFailure(store, &state, 0, fmt.Errorf("retire exact repair branch %s: %w", entry.Branch, err))
		}
		if err := store.AuditStrict(AuditEntry{Action: "repair_retirement_completed", Allowed: true, Repository: entry.Repository, Detail: repairRetirementDetail(*entry)}); err != nil {
			return err
		}
		state.Entries = append([]RepairBranchRetirement(nil), state.Entries[1:]...)
		if err := store.SaveRepairBranchRetirements(state); err != nil {
			return err
		}
	}
	return nil
}

func validateRepairRetirementBinding(config Config, lifecycle *visualhive.LifecycleStore, entry RepairBranchRetirement) error {
	if !strings.EqualFold(config.Repository, entry.Repository) || strings.TrimSpace(config.RepositoryID) != entry.RepositoryID {
		return fmt.Errorf("repair retirement %s does not match the installed repository identity", entry.ID)
	}
	finding, exists := lifecycle.Finding(entry.RepositoryFingerprint)
	if !exists || !strings.EqualFold(finding.Repository, entry.Repository) || finding.RepositoryFingerprint != entry.RepositoryFingerprint {
		return fmt.Errorf("repair retirement %s lost its exact lifecycle finding", entry.ID)
	}
	if entry.Purpose == repairRetirementPurposeMerge {
		if finding.PRNumber != entry.PRNumber || finding.Branch != entry.Branch || !strings.EqualFold(finding.RepairCommitSHA, entry.HeadSHA) || !strings.EqualFold(finding.MergeSHA, entry.MergeSHA) {
			return fmt.Errorf("merged repair retirement %s no longer matches the exact finding PR, branch, head, and merge", entry.ID)
		}
	} else if finding.PRNumber != entry.RetainedPRNumber {
		return fmt.Errorf("superseded repair retirement %s no longer matches retained repair PR #%d", entry.ID, entry.RetainedPRNumber)
	}
	if repairActor(finding.OwningAgentHint) != entry.Agent || finding.RepairAttempts != entry.RepairAttempts {
		return fmt.Errorf("repair retirement %s no longer matches its exact agent and repair-attempt authority", entry.ID)
	}
	return nil
}

func authorizeRepairRetirement(store *Store, lifecycle *visualhive.LifecycleStore, config Config, policy automation.Policy, entry RepairBranchRetirement, action automation.Action, uninstall bool) error {
	allowed := uninstall || entry.Purpose == repairRetirementPurposeMerge
	reasons := []string{"authenticated setup authorizer approved uninstall drain"}
	if entry.Purpose == repairRetirementPurposeMerge && !uninstall {
		// Branch retirement is a mandatory, exact-resource consequence of the
		// already-authorized Hive merge. A later authority downgrade must stop
		// new repairs without stranding that merged branch or its issue closure.
		reasons = []string{"exact Hive-authorized merged repair requires owned branch retirement"}
	} else if !uninstall {
		decision := policy.Authorize(automation.ActionRequest{Action: action, Agent: entry.Agent, Repository: entry.Repository, RepairAttempts: entry.RepairAttempts})
		allowed, reasons = decision.Allowed, decision.Reasons
	}
	detail := repairRetirementDetail(entry) + " reasons=" + strings.Join(reasons, "; ")
	lifecycle.RecordAuthorization(entry.RepositoryFingerprint, string(action), allowed, detail)
	auditAction := "authorize_repair_retirement_" + string(action)
	if uninstall {
		auditAction = "authorize_uninstall_repair_retirement_" + string(action)
	}
	if err := store.AuditStrict(AuditEntry{Action: auditAction, Allowed: allowed, Repository: config.Repository, Detail: detail}); err != nil {
		return err
	}
	if !allowed {
		return fmt.Errorf("%s denied for durable repair retirement %s: %s", action, entry.ID, strings.Join(reasons, "; "))
	}
	return nil
}

func rememberRepairRetirementFailure(store *Store, state *RepairBranchRetirementState, index int, failure error) error {
	entry := &state.Entries[index]
	entry.Attempts++
	entry.LastAttemptAt = time.Now().UTC()
	entry.LastError = failure.Error()
	if len(entry.LastError) > maxRepairRetirementError {
		entry.LastError = entry.LastError[:maxRepairRetirementError]
		for !utf8.ValidString(entry.LastError) {
			entry.LastError = entry.LastError[:len(entry.LastError)-1]
		}
	}
	if saveErr := store.SaveRepairBranchRetirements(*state); saveErr != nil {
		return fmt.Errorf("%v; persist repair retirement failure: %w", failure, saveErr)
	}
	if auditErr := store.AuditStrict(AuditEntry{Action: "repair_retirement_failed", Allowed: false, Repository: entry.Repository, Detail: repairRetirementDetail(*entry) + " error=" + entry.LastError}); auditErr != nil {
		return fmt.Errorf("%v; audit repair retirement failure: %w", failure, auditErr)
	}
	return failure
}

func repairRetirementDetail(entry RepairBranchRetirement) string {
	return fmt.Sprintf("id=%s purpose=%s phase=%s repository_id=%s finding=%s pr=%d retained_pr=%d branch=%s head=%s merge=%s", entry.ID, entry.Purpose, entry.Phase, entry.RepositoryID, entry.RepositoryFingerprint, entry.PRNumber, entry.RetainedPRNumber, entry.Branch, entry.HeadSHA, entry.MergeSHA)
}

func pendingRepairRetirementCount(store *Store) (int, error) {
	state, exists, err := store.LoadRepairBranchRetirements()
	if err != nil || !exists {
		return 0, err
	}
	return len(state.Entries), nil
}

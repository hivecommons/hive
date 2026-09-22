// Package audit runs the report-only convergence audit campaign fixture.
package audit

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/beads"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/convergence"
	"github.com/hivecommons/hive/pkg/convergence/mutation"
	"github.com/hivecommons/hive/pkg/convergence/proof"
	"github.com/hivecommons/hive/pkg/effects"
	"github.com/hivecommons/hive/pkg/outputschema"
)

const (
	Actor               = "hive-audit-lane"
	EffectRecordFinding = "hive.record-finding/v1"

	metaKind          = "audit_kind"
	metaCampaign      = "audit_campaign"
	metaComponent     = "audit_component"
	metaFindingState  = "audit_finding_state"
	metaContentHash   = "audit_content_hash"
	metaFileSet       = "audit_file_set"
	metaDuplicateOf   = "duplicate_of"
	metaReceiptDigest = "receipt_digest"
)

type GitHubClient interface{ Count() int }

type SoakRecorder interface {
	RecordAuditSoak(mode string, generation uint64)
}

type Scope struct {
	Components []Component `json:"components"`
}

type Component struct {
	Name     string    `json:"name"`
	Files    []string  `json:"files"`
	Content  string    `json:"content"`
	Findings []Finding `json:"findings"`
}

type Finding struct {
	Title string   `json:"title"`
	Files []string `json:"files"`
}

type Options struct {
	CampaignKey              string
	ScopeDir                 string
	Store                    *beads.Store
	Ledger                   *mutation.Ledger
	Journal                  *mutation.Journal
	ProofStore               *proof.Store
	Mode                     string
	Generation               uint64
	Holder                   string
	Now                      func() time.Time
	GitHub                   GitHubClient
	Soak                     SoakRecorder
	CrashAfterBeginComponent string
	AfterRecordFindings      func(component string) error
}

type Result struct {
	Findings   []FindingResult
	Receipts   []outputschema.StageReceipt
	Burndown   Burndown
	Mode       string
	Generation uint64
}

type FindingResult struct{ Title, State, BeadID, DuplicateOf string }

type Burndown struct {
	ScopeTotal           int      `json:"scope_total"`
	SatisfiedObligations *int     `json:"satisfied_obligations"`
	KnownRemainingWork   int      `json:"known_remaining_work"`
	UnknownEvidence      []string `json:"unknown_evidence"`
	NullReason           string   `json:"null_reason,omitempty"`
	ScopeAdded           []string `json:"scope_added,omitempty"`
	ScopeRetired         []string `json:"scope_retired,omitempty"`
}

func Run(opts Options) (Result, error) {
	if os.Getenv("HIVE_GITHUB_TOKEN") != "" {
		return Result{}, fmt.Errorf("audit campaign requires HIVE_GITHUB_TOKEN to be unset")
	}
	if opts.Store == nil || opts.Ledger == nil || opts.Journal == nil {
		return Result{}, fmt.Errorf("audit campaign requires existing bead store, mutation ledger, and journal")
	}
	if opts.CampaignKey == "" {
		opts.CampaignKey = "audit-campaign"
	}
	if opts.Holder == "" {
		opts.Holder = Actor
	}
	if opts.Mode == "" {
		opts.Mode = config.ConvergenceModeShadow
	}
	if opts.Generation == 0 {
		opts.Generation = 1
	}
	now := opts.now()
	scope, err := LoadScope(opts.ScopeDir)
	if err != nil {
		return Result{}, err
	}

	if _, err := ensureBead(opts.Store, "campaign", opts.CampaignKey, opts.CampaignKey, nil); err != nil {
		return Result{}, err
	}
	if _, err := ensureBead(opts.Store, "publication", opts.CampaignKey, opts.CampaignKey, map[string]string{"publication_state": "none"}); err != nil {
		return Result{}, err
	}
	claim := mutation.TaskClaim("hivecommons/hive", "hivecommons/hive!"+opts.CampaignKey)
	entry, err := opts.Ledger.Acquire(claim, opts.Holder, time.Hour, now)
	if err != nil {
		return Result{}, err
	}

	res := Result{Mode: opts.Mode, Generation: opts.Generation}
	inspected := 0
	var unknown []string
	seen := existingFindingKeys(opts.Store, opts.CampaignKey)
	for _, c := range scope.Components {
		componentHash := contentHash(c)
		inspection, err := ensureBead(opts.Store, "inspection", opts.CampaignKey, c.Name, map[string]string{metaContentHash: componentHash})
		if err != nil {
			return Result{}, err
		}
		effect := mutation.Effect{
			OutcomeKey:        "hivecommons/hive@" + opts.CampaignKey,
			DesiredGeneration: int(opts.Generation),
			Transition:        "audit-inspection",
			Subject:           "hivecommons/hive!" + opts.CampaignKey + ":" + c.Name,
			ClaimKey:          claim.Key(),
			Kind:              EffectRecordFinding,
			Inputs:            map[string]string{"campaign": opts.CampaignKey, "component": c.Name, "content_hash": componentHash},
		}
		if opts.CrashAfterBeginComponent == c.Name {
			op, err := opts.Journal.Begin(effect, entry.Epoch, opts.Holder, now)
			if err != nil && !errors.Is(err, mutation.ErrNeedsReconciliation) {
				return Result{}, err
			}
			id := effect.LogicalID()
			if op.LogicalID != "" {
				id = op.LogicalID
			}
			if _, err := opts.Journal.Reconcile(id, mutation.ExternalState{Known: false}, now); err != nil {
				return Result{}, err
			}
			_ = opts.Store.SetMetadata(inspection.ID, "inspection_state", string(convergence.ConditionUnknown))
			unknown = append(unknown, c.Name)
			continue
		}
		if prior, ok := opts.Journal.Get(effect.LogicalID()); ok && (prior.Status == mutation.StatusPlanned || prior.Status == mutation.StatusUnknown) {
			if componentEffectApplied(opts.Store, opts.CampaignKey, c.Name, componentHash) {
				if _, err := opts.Journal.Reconcile(effect.LogicalID(), mutation.ExternalState{Known: true, Applied: true, Result: "component=" + c.Name}, now); err != nil {
					return Result{}, err
				}
				receipt := stageReceipt(opts.CampaignKey, c, opts.Generation, componentHash, now)
				res.Receipts = append(res.Receipts, receipt)
				_ = opts.Store.SetMetadata(inspection.ID, "inspection_state", "inspected")
				_ = opts.Store.SetMetadata(inspection.ID, metaReceiptDigest, receipt.OutputDigest)
				inspected++
				continue
			}
			_, _ = opts.Journal.Reconcile(effect.LogicalID(), mutation.ExternalState{Known: true, Applied: false}, now)
		} else if ok && prior.Status == mutation.StatusApplied {
			receipt := stageReceipt(opts.CampaignKey, c, opts.Generation, componentHash, now)
			res.Receipts = append(res.Receipts, receipt)
			_ = opts.Store.SetMetadata(inspection.ID, "inspection_state", "inspected")
			_ = opts.Store.SetMetadata(inspection.ID, metaReceiptDigest, receipt.OutputDigest)
			inspected++
			continue
		}
		executor := mutation.Executor{Ledger: opts.Ledger, Journal: opts.Journal, Mode: opts.Mode, Now: opts.now}
		_, err = executor.Execute(effect, entry.Epoch, opts.Holder, func() (string, error) {
			made, err := recordFindings(opts.Store, opts.CampaignKey, c, componentHash, seen, &res)
			if err != nil {
				return "", err
			}
			if opts.AfterRecordFindings != nil {
				if err := opts.AfterRecordFindings(c.Name); err != nil {
					return "", err
				}
			}
			return fmt.Sprintf("component=%s findings=%d", c.Name, made), nil
		})
		if err != nil {
			return Result{}, err
		}
		receipt := stageReceipt(opts.CampaignKey, c, opts.Generation, componentHash, now)
		res.Receipts = append(res.Receipts, receipt)
		_ = opts.Store.SetMetadata(inspection.ID, "inspection_state", "inspected")
		_ = opts.Store.SetMetadata(inspection.ID, metaReceiptDigest, receipt.OutputDigest)
		if opts.ProofStore != nil {
			_, err = opts.ProofStore.Put(proof.Record{Fingerprint: proof.Fingerprint{OutcomeKey: "hivecommons/hive@" + opts.CampaignKey, PredicateID: proof.PredicateInspectionRecorded, DesiredGeneration: int(opts.Generation), Producer: proof.ProducerHiveAuditLane, InspectionBeadID: inspection.ID, ReceiptDigest: receipt.OutputDigest}, Result: proof.ResultSuccess, Provenance: proof.Provenance{Query: "audit-inspection@" + c.Name}, ObservedAt: now})
			if err != nil {
				return Result{}, err
			}
		}
		inspected++
	}
	if opts.Soak != nil {
		opts.Soak.RecordAuditSoak(opts.Mode, opts.Generation)
	}
	res.Burndown = Burndown{ScopeTotal: len(scope.Components), KnownRemainingWork: len(scope.Components) - inspected, UnknownEvidence: unknown}
	if len(unknown) > 0 {
		res.Burndown.NullReason = "unknown inspection evidence"
	} else {
		v := inspected
		res.Burndown.SatisfiedObligations = &v
	}
	return res, nil
}

func (o Options) now() time.Time {
	if o.Now != nil {
		return o.Now()
	}
	return time.Now().UTC()
}

func LoadScope(dir string) (Scope, error) {
	data, err := os.ReadFile(filepath.Join(dir, "components.json"))
	if err != nil {
		return Scope{}, err
	}
	var scope Scope
	if err := json.Unmarshal(data, &scope); err != nil {
		return Scope{}, err
	}
	if len(scope.Components) == 0 {
		return Scope{}, fmt.Errorf("audit scope is empty")
	}
	return scope, nil
}

func ensureBead(store *beads.Store, kind, campaign, name string, extra map[string]string) (*beads.Bead, error) {
	for _, b := range store.List(beads.ListFilter{}) {
		if b.Meta(metaKind) == kind && b.Meta(metaCampaign) == campaign && b.Meta(metaComponent) == name {
			return b, nil
		}
	}
	b, err := store.Create(kind+": "+name, beads.TypeTask, beads.PriorityMedium, Actor, campaign+":"+name)
	if err != nil {
		return nil, err
	}
	meta := map[string]string{metaKind: kind, metaCampaign: campaign, metaComponent: name}
	for k, v := range extra {
		meta[k] = v
	}
	for k, v := range meta {
		_ = store.SetMetadata(b.ID, k, v)
	}
	return b, nil
}

func recordFindings(store *beads.Store, campaign string, c Component, componentHash string, seen map[string]string, res *Result) (int, error) {
	made := 0
	for _, f := range c.Findings {
		key := duplicateKey(f.Title, f.Files)
		state := "validated"
		dupOf := ""
		if first, ok := seen[key]; ok {
			state, dupOf = "duplicate_of", first
		} else {
			seen[key] = ""
		}
		b, err := store.Create("finding: "+f.Title, beads.TypeAdvisory, beads.PriorityMedium, Actor, campaign+":"+c.Name)
		if err != nil {
			return made, err
		}
		_ = store.SetMetadata(b.ID, metaKind, "finding")
		_ = store.SetMetadata(b.ID, metaCampaign, campaign)
		_ = store.SetMetadata(b.ID, metaComponent, c.Name)
		_ = store.SetMetadata(b.ID, metaContentHash, componentHash)
		_ = store.SetMetadata(b.ID, metaFindingState, state)
		_ = store.SetMetadata(b.ID, metaFileSet, strings.Join(normalizeFiles(f.Files), ","))
		if state == "duplicate_of" {
			_ = store.SetMetadata(b.ID, metaDuplicateOf, dupOf)
		} else {
			seen[key] = b.ID
		}
		res.Findings = append(res.Findings, FindingResult{Title: f.Title, State: state, BeadID: b.ID, DuplicateOf: dupOf})
		made++
	}
	return made, nil
}

func componentEffectApplied(store *beads.Store, campaign, component, componentHash string) bool {
	for _, b := range store.List(beads.ListFilter{}) {
		if b.Meta(metaKind) == "finding" && b.Meta(metaCampaign) == campaign &&
			b.Meta(metaComponent) == component && b.Meta(metaContentHash) == componentHash {
			return true
		}
	}
	return false
}

func existingFindingKeys(store *beads.Store, campaign string) map[string]string {
	out := map[string]string{}
	for _, b := range store.List(beads.ListFilter{}) {
		if b.Meta(metaKind) != "finding" || b.Meta(metaCampaign) != campaign || b.Meta(metaFindingState) != "validated" {
			continue
		}
		title := strings.TrimPrefix(b.Title, "finding: ")
		out[duplicateKey(title, strings.Split(b.Meta(metaFileSet), ","))] = b.ID
	}
	return out
}

func contentHash(c Component) string {
	return stableHash(c.Name, strings.Join(c.Files, "\x00"), c.Content)
}
func stableHash(parts ...string) string {
	h := sha256.New()
	for _, p := range parts {
		h.Write([]byte(p))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

func duplicateKey(title string, files []string) string {
	return normalizeTitle(title) + "|" + strings.Join(normalizeFiles(files), "\x00")
}
func normalizeTitle(title string) string {
	return strings.Join(strings.Fields(strings.ToLower(title)), " ")
}
func normalizeFiles(files []string) []string {
	out := append([]string(nil), files...)
	sort.Strings(out)
	return out
}

func stageReceipt(campaign string, c Component, generation uint64, input string, now time.Time) outputschema.StageReceipt {
	artifacts := []outputschema.Artifact{{Repo: "hivecommons/hive", Path: "audit-scope/" + c.Name, Description: "report-only audit inspection receipt"}}
	parts := make([]string, 0, len(artifacts))
	for _, artifact := range artifacts {
		parts = append(parts, strings.Join([]string{artifact.Repo, artifact.Path, artifact.Description}, "\x00"))
	}
	sort.Strings(parts)
	return outputschema.StageReceipt{SchemaVersion: outputschema.StageReceiptSchemaVersion, WorkKey: "hivecommons/hive!" + campaign + ":" + c.Name, AssignmentID: campaign + ":" + c.Name, Generation: generation, Stage: "audit-inspection", ContractRevision: proof.PredicateInspectionRecorded, ExecutionKey: mutation.DeriveLogicalID([]string{campaign, c.Name, input}, nil), Engine: &outputschema.StageReceiptEngine{Name: Actor, Version: "v1"}, InputRevision: "artifact@" + input, OutputDigest: effects.StableDigest(parts...), ResultClass: outputschema.ReceiptResultCompleted, StartedAt: now.Format(time.RFC3339Nano), EndedAt: now.Format(time.RFC3339Nano), Provenance: &proof.Provenance{Query: "audit-scope/" + c.Name}, Artifacts: artifacts}
}

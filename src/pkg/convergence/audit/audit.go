// Package audit runs the report-only convergence audit campaign fixture.
package audit

import (
	"context"
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
	"github.com/hivecommons/hive/pkg/convergence/outcome"
	"github.com/hivecommons/hive/pkg/convergence/proof"
	"github.com/hivecommons/hive/pkg/convergence/publish"
	"github.com/hivecommons/hive/pkg/effects"
	"github.com/hivecommons/hive/pkg/findingidentity"
	"github.com/hivecommons/hive/pkg/outputschema"
)

const (
	Actor               = "hive-audit-lane"
	EffectRecordFinding = "hive.record-finding/v1"

	// auditRepo is the repository the pilot fixture inspects and would
	// publish into.
	auditRepo = "hivecommons/hive"

	metaKind          = "audit_kind"
	metaCampaign      = "audit_campaign"
	metaComponent     = "audit_component"
	metaFindingState  = "audit_finding_state"
	metaContentHash   = "audit_content_hash"
	metaFindingHash   = "audit_finding_hash"
	metaLabels        = "audit_labels"
	metaFileSet       = "audit_file_set"
	metaDuplicateOf   = "duplicate_of"
	metaReceiptDigest = "receipt_digest"
	// metaPublication records the publisher's verdict on a finding bead and
	// the campaign summary on the publication bead.
	metaPublication = "publication_state"

	// publicationNone is the publication bead value while nothing publishes.
	publicationNone = "none"
	// findingTitlePrefix is what finding bead titles start with.
	findingTitlePrefix = "finding: "
	// labelSeparator joins finding labels in bead metadata.
	labelSeparator = ","
)

// FindingPublisher is the publication gate the campaign hands its validated
// findings to (#8353). It is satisfied by *publish.Publisher; nil keeps the
// campaign report-only and the publication bead at "none".
type FindingPublisher interface {
	PublishCampaign(ctx context.Context, c publish.Campaign, findings []publish.Finding, grant publish.Grant, ledger *outcome.Ledger) (publish.CampaignResult, error)
}

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
	Title         string   `json:"title"`
	Files         []string `json:"files"`
	Labels        []string `json:"labels,omitempty"`
	SubjectDigest string   `json:"subject_digest,omitempty"`
	Predicate     string   `json:"predicate,omitempty"`
	Location      string   `json:"location,omitempty"`
	EvidenceRefs  []string `json:"evidence_refs,omitempty"`
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
	// Publisher, when set, receives every finding of the campaign after
	// inspection; nil leaves publication at "none".
	Publisher FindingPublisher
	// Outcomes is the outcome ledger the publisher books the campaign's
	// predicted end state on; nil publishes without an outcome record.
	Outcomes *outcome.Ledger
	// RunKey and RunURL identify the campaign run on published bodies.
	RunKey string
	RunURL string
}

type Result struct {
	Findings   []FindingResult
	Receipts   []outputschema.StageReceipt
	Burndown   Burndown
	Mode       string
	Generation uint64
	// Publication is the publisher's campaign result, nil when no publisher
	// is wired.
	Publication *publish.CampaignResult
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
	publication, err := ensureBead(opts.Store, "publication", opts.CampaignKey, opts.CampaignKey, map[string]string{metaPublication: publicationNone})
	if err != nil {
		return Result{}, err
	}
	claim := mutation.TaskClaim(auditRepo, auditRepo+"!"+opts.CampaignKey)
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
			OutcomeKey:        auditRepo + "@" + opts.CampaignKey,
			DesiredGeneration: int(opts.Generation),
			Transition:        "audit-inspection",
			Subject:           auditRepo + "!" + opts.CampaignKey + ":" + c.Name,
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
			_, err = opts.ProofStore.Put(proof.Record{Fingerprint: proof.Fingerprint{OutcomeKey: auditRepo + "@" + opts.CampaignKey, PredicateID: proof.PredicateInspectionRecorded, DesiredGeneration: int(opts.Generation), Producer: proof.ProducerHiveAuditLane, InspectionBeadID: inspection.ID, ReceiptDigest: receipt.OutputDigest}, Result: proof.ResultSuccess, Provenance: proof.Provenance{Query: "audit-inspection@" + c.Name}, ObservedAt: now})
			if err != nil {
				return Result{}, err
			}
		}
		inspected++
	}
	if opts.Publisher != nil {
		pubRes, err := opts.Publisher.PublishCampaign(context.Background(),
			publish.Campaign{Key: opts.CampaignKey, Repo: auditRepo, RunKey: opts.RunKey, RunURL: opts.RunURL},
			campaignFindings(opts.Store, opts.CampaignKey),
			publish.Grant{ClaimKey: claim.Key(), Epoch: entry.Epoch, Holder: opts.Holder}, opts.Outcomes)
		res.Publication = &pubRes
		recordPublications(opts.Store, opts.CampaignKey, publication.ID, pubRes)
		if err != nil {
			return res, err
		}
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
		key := duplicateKey(f)
		state := "validated"
		dupOf := ""
		if first, ok := seen[key]; ok {
			state, dupOf = "duplicate_of", first
		} else {
			seen[key] = ""
		}
		b, err := store.Create(findingTitlePrefix+f.Title, beads.TypeAdvisory, beads.PriorityMedium, Actor, campaign+":"+c.Name)
		if err != nil {
			return made, err
		}
		_ = store.SetMetadata(b.ID, metaKind, "finding")
		_ = store.SetMetadata(b.ID, metaCampaign, campaign)
		_ = store.SetMetadata(b.ID, metaComponent, c.Name)
		_ = store.SetMetadata(b.ID, metaContentHash, componentHash)
		_ = store.SetMetadata(b.ID, metaFindingHash, findingHash(componentHash, key))
		_ = store.SetMetadata(b.ID, metaFindingState, state)
		_ = store.SetMetadata(b.ID, metaFileSet, strings.Join(normalizeFiles(f.Files), labelSeparator))
		if identityKey := findingKey(f); identityKey != "" {
			_ = store.SetMetadata(b.ID, findingidentity.MetaKey, identityKey)
			_ = store.SetMetadata(b.ID, findingidentity.MetaSubjectDigest, f.SubjectDigest)
			_ = store.SetMetadata(b.ID, findingidentity.MetaPredicate, f.Predicate)
			_ = store.SetMetadata(b.ID, findingidentity.MetaLocation, f.Location)
		}
		if len(f.Labels) > 0 {
			_ = store.SetMetadata(b.ID, metaLabels, strings.Join(f.Labels, labelSeparator))
		}
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

// storedFinding reconstructs the scope-level finding from its bead so the
// same duplicate key (semantic identity when recorded, title+files otherwise)
// is derived on every read.
func storedFinding(b *beads.Bead) Finding {
	return Finding{
		Title:         strings.TrimPrefix(b.Title, findingTitlePrefix),
		Files:         splitList(b.Meta(metaFileSet)),
		Labels:        splitList(b.Meta(metaLabels)),
		SubjectDigest: b.Meta(findingidentity.MetaSubjectDigest),
		Predicate:     b.Meta(findingidentity.MetaPredicate),
		Location:      b.Meta(findingidentity.MetaLocation),
	}
}

// findingHash is the finding identity (#8318): the component content hash
// bound to the normalized title and file set, so two findings recorded from
// one component never share a publication identity.
func findingHash(componentHash, duplicateKey string) string {
	return stableHash(componentHash, duplicateKey)
}

// campaignFindings projects the campaign's finding beads into the
// publisher's input, binding each to its component's inspection receipt.
func campaignFindings(store *beads.Store, campaign string) []publish.Finding {
	receipts := map[string]string{}
	var out []publish.Finding
	for _, b := range store.List(beads.ListFilter{}) {
		if b.Meta(metaCampaign) != campaign {
			continue
		}
		if b.Meta(metaKind) == "inspection" {
			receipts[b.Meta(metaComponent)] = b.Meta(metaReceiptDigest)
		}
	}
	for _, b := range store.List(beads.ListFilter{}) {
		if b.Meta(metaKind) != "finding" || b.Meta(metaCampaign) != campaign {
			continue
		}
		stored := storedFinding(b)
		hash := b.Meta(metaFindingHash)
		if hash == "" {
			hash = findingHash(b.Meta(metaContentHash), duplicateKey(stored))
		}
		out = append(out, publish.Finding{
			Campaign:      campaign,
			Repo:          auditRepo,
			BeadID:        b.ID,
			Title:         stored.Title,
			Evidence:      "Recorded by the audit inspection of component `" + b.Meta(metaComponent) + "`.",
			Files:         stored.Files,
			ContentHash:   hash,
			Predicate:     proof.PredicateInspectionRecorded,
			Labels:        stored.Labels,
			ReceiptDigest: receipts[b.Meta(metaComponent)],
			State:         b.Meta(metaFindingState),
			DuplicateOf:   b.Meta(metaDuplicateOf),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ContentHash < out[j].ContentHash })
	return out
}

// recordPublications writes each finding's publication verdict on its bead
// and the campaign summary on the publication bead.
func recordPublications(store *beads.Store, campaign, publicationBeadID string, res publish.CampaignResult) {
	counts := map[string]int{}
	for _, b := range store.List(beads.ListFilter{}) {
		if b.Meta(metaKind) != "finding" || b.Meta(metaCampaign) != campaign {
			continue
		}
		hash := b.Meta(metaFindingHash)
		pub, ok := res.Publications[hash]
		if !ok {
			continue
		}
		_ = store.SetMetadata(b.ID, metaPublication, pub.Record())
		counts[pub.State]++
	}
	states := make([]string, 0, len(counts))
	for state := range counts {
		states = append(states, state)
	}
	sort.Strings(states)
	parts := make([]string, 0, len(states))
	for _, state := range states {
		parts = append(parts, fmt.Sprintf("%s=%d", state, counts[state]))
	}
	summary := strings.Join(parts, " ")
	if summary == "" {
		summary = publicationNone
	}
	_ = store.SetMetadata(publicationBeadID, metaPublication, summary)
}

func splitList(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	return strings.Split(raw, labelSeparator)
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
		out[duplicateKey(storedFinding(b))] = b.ID
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

func duplicateKey(f Finding) string {
	if key := findingKey(f); key != "" {
		return key
	}
	return normalizeTitle(f.Title) + "|" + strings.Join(normalizeFiles(f.Files), "\x00")
}
func findingKey(f Finding) string {
	return findingidentity.Key(findingidentity.Record{
		SubjectDigest: f.SubjectDigest,
		Predicate:     f.Predicate,
		Location:      f.Location,
	})
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

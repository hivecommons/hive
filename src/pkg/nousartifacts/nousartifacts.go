// Package nousartifacts mirrors finished Nous (Experiment Lab) campaign
// folders from the durable Nous root to an optional GitHub artifact
// repository (RFC #11585 part 8, #11701).
//
// The feature is OFF unless nous.artifact_repo.repo names a repository; a
// Syncer built from a disabled configuration makes no network call at all.
// When enabled:
//
//   - One folder per campaign is written below the configured path prefix
//     and delivered as a pull request against the configured branch, so the
//     repository's own review and merge rules apply. Nothing is promoted:
//     a verdict still needs an owner's explicit Promote.
//   - A campaign is mirrored once, when it has ended. "Ended" is a verdict
//     file (meta_findings.json) or no write for QuietPeriod; the latter is
//     recorded as incomplete so crashed and aborted campaigns are kept too.
//   - Pushes go through the hive's own GitHub identity (the App), never an
//     agent's scoped token or the owner's personal token. The mirror is
//     write-only to the one configured repository.
//   - Secrets never leave the hive: a filename denylist drops key, token and
//     environment files, every text file is run through pkg/logscrub, and
//     files over the size caps are omitted and listed in the manifest.
//   - Every push, and every failure, is audited and shown in
//     GET /api/nous/status under "artifact_repo". Failures back off
//     exponentially and the syncer stands down while the GitHub API budget
//     governor is conserving.
package nousartifacts

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/hivecommons/hive/crux/logscrub"
	"github.com/hivecommons/hive/pkg/nousengine"
	"github.com/hivecommons/hive/pkg/nousstore"
)

// Mirror policy constants.
const (
	// QuietPeriod is how long a campaign folder without a verdict must go
	// unwritten before it is treated as ended (crashed or aborted) and
	// mirrored as incomplete.
	QuietPeriod = 30 * time.Minute

	// MaxFileBytes is the largest single file the mirror will push. Larger
	// files are omitted and listed in the manifest; Git LFS is out of scope.
	MaxFileBytes = 2 << 20
	// MaxCommitBytes caps the decoded bytes of one mirror commit. It matches
	// the signed-commit cap in pkg/github so one mutation never runs for
	// minutes on the sync goroutine.
	MaxCommitBytes = 20 << 20
	// MaxFilesPerCommit caps how many files one mirror commit carries.
	MaxFilesPerCommit = 400

	// InitialBackoff is the delay after the first failed sync; it doubles
	// on each consecutive failure up to MaxBackoff and resets on success.
	InitialBackoff = time.Minute
	MaxBackoff     = time.Hour

	// BranchPrefix namespaces the per-campaign PR branches in the artifact
	// repository.
	BranchPrefix = "nous/artifacts/"

	// ManifestFile is written into each mirrored campaign folder and lists
	// what was pushed, what was omitted and whether anything was redacted.
	ManifestFile = "hive-sync.json"

	// StateFile, at the Nous root, remembers what has been mirrored so a
	// campaign is pushed once per content fingerprint.
	StateFile = "artifact-sync-state.json"

	// Campaign statuses recorded in the manifest and status. StatusLedger
	// marks the governor-scope folder (<root>/governor), which is a rolling
	// ledger rather than a campaign and is re-mirrored whenever it changes
	// and then goes quiet.
	StatusCompleted  = "completed"
	StatusIncomplete = "incomplete"
	StatusLedger     = "ledger"

	// Audit actions.
	AuditActionPush       = "nous_artifact_push"
	AuditActionPushFailed = "nous_artifact_push_failed"
	// AuditUser is the actor recorded for mirror pushes: the hive itself.
	AuditUser = "hive"

	// Omission reasons recorded in the manifest.
	OmitReasonDenylist   = "denylisted filename"
	OmitReasonFileSize   = "over per-file size cap"
	OmitReasonCommitSize = "over per-commit size cap"
	OmitReasonFileCount  = "over per-commit file cap"
	OmitReasonSymlink    = "symbolic link"

	dirMode       fs.FileMode = 0o755
	stateFileMode fs.FileMode = 0o644
	tmpSuffix                 = ".tmp"
)

// rootExclusions are entries directly under the Nous root that are never
// campaign folders: regenerable bulk, owner gate decisions and the
// dependency cache.
var rootExclusions = map[string]bool{
	nousstore.SnapshotsSubdir: true,
	nousstore.GatesSubdir:     true,
	"cache":                   true,
}

// deniedDirs are skipped wherever they appear inside a campaign folder.
var deniedDirs = map[string]bool{".git": true, "__pycache__": true, "node_modules": true, ".venv": true, "venv": true}

// deniedNamePattern drops files whose NAME suggests credentials, local
// environment or build output regardless of content. Case-insensitive.
var deniedNamePattern = regexp.MustCompile(`(?i)(^\.|\.tmp$|\.env$|\.pem$|\.key$|\.p12$|\.pfx$|\.jks$|\.keystore$|^id_(rsa|dsa|ecdsa|ed25519)|\.netrc$|\.npmrc$|\.pypirc$|token|secret|credential|password|\.pyc$|\.so$)`)

// branchUnsafe matches characters that cannot appear in a branch name.
var branchUnsafe = regexp.MustCompile(`[^A-Za-z0-9._/-]+`)

// File is one file to push, relative to the repository root.
type File struct {
	Path    string
	Content []byte
}

// PullRequest is what the forge reports after CreatePullRequest.
type PullRequest struct {
	Number         int
	URL            string
	AlreadyExisted bool
}

// Forge is the slice of the hive's GitHub client the mirror needs. cmd/hive
// adapts *github.Client to it; tests use a fake.
type Forge interface {
	DefaultBranch(ctx context.Context, repo string) (string, error)
	BranchHead(ctx context.Context, repo, branch string) (sha string, exists bool, err error)
	CreateBranch(ctx context.Context, repo, branch, fromSHA string) error
	CommitFiles(ctx context.Context, repo, branch, expectedHeadSHA, headline, body string, files []File) (sha string, err error)
	CreatePullRequest(ctx context.Context, repo, head, base, title, body string) (PullRequest, error)
	RepoIsPrivate(ctx context.Context, repo string) (bool, error)
}

// Settings is the resolved artifact-repo configuration plus the Nous root.
type Settings struct {
	// Repo is owner/name; empty disables the mirror.
	Repo string
	// Branch is the PR target; empty resolves the repository default.
	Branch string
	// PathPrefix is the folder below which campaign folders are written.
	PathPrefix string
	// Root is the durable Nous root (<nous_dir>).
	Root string
}

// Enabled reports whether a repository is configured.
func (s Settings) Enabled() bool { return strings.TrimSpace(s.Repo) != "" }

// CampaignRecord is the mirror state of one campaign.
type CampaignRecord struct {
	Status      string    `json:"status"`
	Fingerprint string    `json:"fingerprint"`
	Commit      string    `json:"commit,omitempty"`
	Branch      string    `json:"branch,omitempty"`
	PRNumber    int       `json:"pr_number,omitempty"`
	PRURL       string    `json:"pr_url,omitempty"`
	Files       int       `json:"files"`
	Omitted     int       `json:"omitted,omitempty"`
	Redacted    int       `json:"redacted,omitempty"`
	Bytes       int64     `json:"bytes"`
	SyncedAt    time.Time `json:"synced_at"`
	LastError   string    `json:"last_error,omitempty"`
	LastErrorAt time.Time `json:"last_error_at,omitempty"`
}

// Status is what GET /api/nous/status reports under "artifact_repo".
type Status struct {
	Enabled bool   `json:"enabled"`
	Repo    string `json:"repo,omitempty"`
	Branch  string `json:"branch,omitempty"`
	Path    string `json:"path,omitempty"`
	// Public is set once the repository's visibility is known. A public
	// destination is a warning, never a block (RFC §5).
	Public         *bool                     `json:"public,omitempty"`
	LastAttemptAt  *time.Time                `json:"last_attempt_at,omitempty"`
	LastSyncAt     *time.Time                `json:"last_sync_at,omitempty"`
	LastError      string                    `json:"last_error,omitempty"`
	LastSkipReason string                    `json:"last_skip_reason,omitempty"`
	NextAttemptAt  *time.Time                `json:"next_attempt_at,omitempty"`
	Failures       int                       `json:"consecutive_failures,omitempty"`
	Campaigns      map[string]CampaignRecord `json:"campaigns,omitempty"`
}

// Result summarises one RunOnce.
type Result struct {
	// Skipped is the reason nothing was attempted; empty when a scan ran.
	Skipped string `json:"skipped,omitempty"`
	// Pushed lists campaigns mirrored in this run.
	Pushed []string `json:"pushed,omitempty"`
	// Failed maps campaign → error for campaigns that could not be pushed.
	Failed map[string]string `json:"failed,omitempty"`
	// Unchanged counts campaigns already mirrored at their current content.
	Unchanged int `json:"unchanged"`
	// Running counts campaigns that have not ended yet.
	Running int `json:"running"`
}

// Syncer mirrors campaign folders. Build one with New; the zero value is
// unusable. All exported methods are safe for concurrent use.
type Syncer struct {
	settings Settings
	forge    Forge
	logger   *slog.Logger

	// Now is the clock; nil means time.Now.
	Now func() time.Time
	// BudgetOK reports whether the GitHub API budget allows optional work.
	// nil means always. A false return skips the scan and records reason.
	BudgetOK func() (ok bool, reason string)
	// Audit records a push or a failure; nil disables auditing.
	Audit func(action, detail, repo string, prNumber int)
	// Publish receives the status after every change; nil disables.
	Publish func(Status)

	mu            sync.Mutex
	runMu         sync.Mutex
	status        Status
	state         map[string]CampaignRecord
	backoff       time.Duration
	defaultBranch string
}

// New builds a Syncer. The forge may be nil when settings are disabled.
func New(settings Settings, forge Forge, logger *slog.Logger) *Syncer {
	if logger == nil {
		logger = slog.Default()
	}
	settings.Repo = strings.TrimSpace(settings.Repo)
	settings.Branch = strings.TrimSpace(settings.Branch)
	settings.PathPrefix = strings.Trim(path.Clean(strings.ReplaceAll(settings.PathPrefix, "\\", "/")), "/")
	if settings.PathPrefix == "." {
		settings.PathPrefix = ""
	}
	s := &Syncer{settings: settings, forge: forge, logger: logger, state: map[string]CampaignRecord{}}
	s.status = Status{Enabled: settings.Enabled(), Repo: settings.Repo, Branch: settings.Branch, Path: settings.PathPrefix}
	if settings.Enabled() {
		s.loadState()
	}
	return s
}

// Enabled reports whether this syncer will ever contact GitHub.
func (s *Syncer) Enabled() bool { return s != nil && s.settings.Enabled() }

// Status returns a copy of the current status.
func (s *Syncer) Status() Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.snapshotLocked()
}

func (s *Syncer) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Syncer) statePath() string { return filepath.Join(s.settings.Root, StateFile) }

func (s *Syncer) loadState() {
	data, err := os.ReadFile(s.statePath())
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			s.logger.Warn("nous artifact mirror: cannot read state; every campaign will be re-evaluated", "path", s.statePath(), "error", err)
		}
		return
	}
	var saved map[string]CampaignRecord
	if err := json.Unmarshal(data, &saved); err != nil {
		s.logger.Warn("nous artifact mirror: corrupt state ignored", "path", s.statePath(), "error", err)
		return
	}
	if saved != nil {
		s.state = saved
	}
}

// saveStateLocked writes the state atomically. Failure is logged, not fatal:
// the worst case is a repeated push of identical content on the next scan.
func (s *Syncer) saveStateLocked() {
	data, err := json.MarshalIndent(s.state, "", "  ")
	if err != nil {
		s.logger.Warn("nous artifact mirror: encode state", "error", err)
		return
	}
	if err := os.MkdirAll(s.settings.Root, dirMode); err != nil {
		s.logger.Warn("nous artifact mirror: create root", "error", err)
		return
	}
	tmp := s.statePath() + tmpSuffix
	if err := os.WriteFile(tmp, append(data, '\n'), stateFileMode); err != nil {
		s.logger.Warn("nous artifact mirror: write state", "error", err)
		return
	}
	if err := os.Rename(tmp, s.statePath()); err != nil {
		_ = os.Remove(tmp)
		s.logger.Warn("nous artifact mirror: rename state", "error", err)
	}
}

func (s *Syncer) snapshotLocked() Status {
	out := s.status
	out.Campaigns = make(map[string]CampaignRecord, len(s.state))
	for k, v := range s.state {
		out.Campaigns[k] = v
	}
	return out
}

func (s *Syncer) publishLocked() {
	if s.Publish != nil {
		s.Publish(s.snapshotLocked())
	}
}

// RunOnce scans the Nous root and mirrors every ended campaign whose content
// changed since it was last pushed. force bypasses the failure backoff (an
// owner's "Sync now") but never the API budget governor. A disabled syncer
// returns immediately with Skipped set and touches neither disk nor network.
// Concurrent calls serialise; the second waits for the first.
func (s *Syncer) RunOnce(ctx context.Context, force bool) Result {
	if !s.Enabled() {
		return Result{Skipped: "artifact repo not configured"}
	}
	if s.forge == nil {
		return s.skip("no GitHub client (hive is running without GitHub credentials)")
	}
	s.runMu.Lock()
	defer s.runMu.Unlock()

	now := s.now()
	s.mu.Lock()
	if !force && s.status.NextAttemptAt != nil && now.Before(*s.status.NextAttemptAt) {
		reason := "backing off until " + s.status.NextAttemptAt.UTC().Format(time.RFC3339)
		s.status.LastSkipReason = reason
		s.publishLocked()
		s.mu.Unlock()
		return Result{Skipped: reason}
	}
	s.mu.Unlock()
	if s.BudgetOK != nil {
		if ok, reason := s.BudgetOK(); !ok {
			return s.skip("github api budget: " + reason)
		}
	}

	s.mu.Lock()
	s.status.LastAttemptAt = &now
	s.status.LastSkipReason = ""
	s.mu.Unlock()
	s.checkVisibility(ctx)

	campaigns, err := s.discover(now)
	if err != nil {
		return s.fail(now, fmt.Errorf("scanning %s: %w", s.settings.Root, err))
	}
	res := Result{}
	for _, c := range campaigns {
		if c.status == "" {
			res.Running++
			continue
		}
		bundle, err := s.collect(c)
		if err != nil {
			res.addFailure(c.id, err)
			s.recordFailure(now, c.id, err)
			continue
		}
		s.mu.Lock()
		prev, seen := s.state[c.id]
		s.mu.Unlock()
		if seen && prev.Fingerprint == bundle.fingerprint && prev.Commit != "" {
			res.Unchanged++
			continue
		}
		if err := s.push(ctx, now, c, bundle); err != nil {
			res.addFailure(c.id, err)
			s.recordFailure(now, c.id, err)
			continue
		}
		res.Pushed = append(res.Pushed, c.id)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(res.Failed) > 0 {
		s.applyBackoffLocked(now)
		s.status.LastError = summarizeFailures(res.Failed)
	} else {
		s.backoff = 0
		s.status.Failures = 0
		s.status.NextAttemptAt = nil
		s.status.LastError = ""
		s.status.LastSyncAt = &now
	}
	s.saveStateLocked()
	s.publishLocked()
	return res
}

func (r *Result) addFailure(campaign string, err error) {
	if r.Failed == nil {
		r.Failed = map[string]string{}
	}
	r.Failed[campaign] = err.Error()
}

func (s *Syncer) skip(reason string) Result {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.status.LastSkipReason = reason
	s.publishLocked()
	return Result{Skipped: reason}
}

// fail handles a run-level error (the root could not be scanned).
func (s *Syncer) fail(now time.Time, err error) Result {
	s.recordFailure(now, "", err)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.applyBackoffLocked(now)
	s.status.LastError = err.Error()
	s.publishLocked()
	return Result{Skipped: err.Error()}
}

func (s *Syncer) recordFailure(now time.Time, campaign string, err error) {
	s.logger.Warn("nous artifact mirror: push failed", "campaign", campaign, "repo", s.settings.Repo, "error", err)
	if campaign != "" {
		s.mu.Lock()
		rec := s.state[campaign]
		rec.LastError = err.Error()
		rec.LastErrorAt = now
		s.state[campaign] = rec
		s.mu.Unlock()
	}
	if s.Audit != nil {
		s.Audit(AuditActionPushFailed, fmt.Sprintf("campaign=%s, error=%s", campaign, logscrub.ScrubString(err.Error())), s.settings.Repo, 0)
	}
}

func (s *Syncer) applyBackoffLocked(now time.Time) {
	switch {
	case s.backoff == 0:
		s.backoff = InitialBackoff
	case s.backoff < MaxBackoff:
		s.backoff *= 2
		if s.backoff > MaxBackoff {
			s.backoff = MaxBackoff
		}
	}
	s.status.Failures++
	next := now.Add(s.backoff)
	s.status.NextAttemptAt = &next
}

// checkVisibility learns the destination's visibility once and warns when it
// is public. A lookup failure is logged and retried on the next run.
func (s *Syncer) checkVisibility(ctx context.Context) {
	s.mu.Lock()
	known := s.status.Public != nil
	s.mu.Unlock()
	if known {
		return
	}
	private, err := s.forge.RepoIsPrivate(ctx, s.settings.Repo)
	if err != nil {
		s.logger.Warn("nous artifact mirror: cannot determine repository visibility", "repo", s.settings.Repo, "error", err)
		return
	}
	public := !private
	if public {
		s.logger.Warn("nous artifact mirror: destination repository is PUBLIC; campaign artifacts will be visible to everyone", "repo", s.settings.Repo)
	}
	s.mu.Lock()
	s.status.Public = &public
	s.mu.Unlock()
}

// campaign is one folder under the root. status is "" while it is running.
type campaign struct {
	id     string
	dir    string
	status string
}

// discover lists campaign folders directly under the root in name order.
// Empty folders are ignored.
func (s *Syncer) discover(now time.Time) ([]campaign, error) {
	entries, err := os.ReadDir(s.settings.Root)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []campaign
	for _, e := range entries {
		name := e.Name()
		if !e.IsDir() || rootExclusions[name] || strings.HasPrefix(name, ".") {
			continue
		}
		dir := filepath.Join(s.settings.Root, name)
		if !hasAnyFile(dir) {
			continue
		}
		status, err := campaignStatus(name, dir, now)
		if err != nil {
			return nil, err
		}
		out = append(out, campaign{id: name, dir: dir, status: status})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].id < out[j].id })
	return out, nil
}

// campaignStatus is StatusCompleted when a verdict exists, StatusIncomplete
// (or StatusLedger for the governor folder) when nothing in the folder was
// written for QuietPeriod, else "" while the campaign is still running.
func campaignStatus(id, dir string, now time.Time) (string, error) {
	if id != nousstore.GovernorSubdir {
		if _, err := os.Stat(filepath.Join(dir, nousengine.MetaFindingsFile)); err == nil {
			return StatusCompleted, nil
		}
	}
	newest, err := newestMtime(dir)
	if err != nil {
		return "", err
	}
	if now.Sub(newest) < QuietPeriod {
		return "", nil
	}
	if id == nousstore.GovernorSubdir {
		return StatusLedger, nil
	}
	return StatusIncomplete, nil
}

// newestMtime is the newest modification time of any file in dir.
func newestMtime(dir string) (time.Time, error) {
	var newest time.Time
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if info.ModTime().After(newest) {
			newest = info.ModTime()
		}
		return nil
	})
	return newest, err
}

func hasAnyFile(dir string) bool {
	found := false
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			found = true
			return fs.SkipAll
		}
		return nil
	})
	return found
}

// manifest is ManifestFile's content.
type manifest struct {
	Campaign    string           `json:"campaign"`
	Status      string           `json:"status"`
	Fingerprint string           `json:"fingerprint"`
	SyncedAt    time.Time        `json:"synced_at"`
	Files       []manifestFile   `json:"files"`
	Omitted     []manifestOmit   `json:"omitted,omitempty"`
	Note        string           `json:"note"`
	Caps        map[string]int64 `json:"caps"`
}

type manifestFile struct {
	Path     string `json:"path"`
	Bytes    int    `json:"bytes"`
	SHA256   string `json:"sha256"`
	Redacted bool   `json:"redacted,omitempty"`
}

type manifestOmit struct {
	Path   string `json:"path"`
	Reason string `json:"reason"`
	Bytes  int64  `json:"bytes,omitempty"`
}

// bundle is a campaign's filtered, redacted files ready to push.
type bundle struct {
	files       []File // repo-relative paths, manifest excluded
	manifest    manifest
	fingerprint string
	bytes       int64
	redacted    int
}

// collect walks a campaign folder applying the denylist, redaction and size
// caps. Candidates are sorted by path before the caps apply, so the result
// and its fingerprint are deterministic for a given folder.
func (s *Syncer) collect(c campaign) (bundle, error) {
	b := bundle{manifest: manifest{Campaign: c.id, Status: c.status, Caps: map[string]int64{"max_file_bytes": MaxFileBytes, "max_commit_bytes": MaxCommitBytes, "max_files": MaxFilesPerCommit}}}
	type candidate struct {
		rel  string
		abs  string
		size int64
	}
	var candidates []candidate
	err := filepath.WalkDir(c.dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(c.dir, p)
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			if p != c.dir && (deniedDirs[d.Name()] || strings.HasPrefix(d.Name(), ".")) {
				return fs.SkipDir
			}
			return nil
		}
		if d.Type()&fs.ModeSymlink != 0 {
			b.manifest.Omitted = append(b.manifest.Omitted, manifestOmit{Path: rel, Reason: OmitReasonSymlink})
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		if rel == ManifestFile || deniedNamePattern.MatchString(d.Name()) {
			b.manifest.Omitted = append(b.manifest.Omitted, manifestOmit{Path: rel, Reason: OmitReasonDenylist})
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if info.Size() > MaxFileBytes {
			b.manifest.Omitted = append(b.manifest.Omitted, manifestOmit{Path: rel, Reason: OmitReasonFileSize, Bytes: info.Size()})
			return nil
		}
		candidates = append(candidates, candidate{rel: rel, abs: p, size: info.Size()})
		return nil
	})
	if err != nil {
		return bundle{}, fmt.Errorf("walking %s: %w", c.dir, err)
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].rel < candidates[j].rel })
	sort.Slice(b.manifest.Omitted, func(i, j int) bool { return b.manifest.Omitted[i].Path < b.manifest.Omitted[j].Path })

	hasher := sha256.New()
	hasher.Write([]byte(c.status + "\n"))
	for _, cand := range candidates {
		if len(b.files) >= MaxFilesPerCommit {
			b.manifest.Omitted = append(b.manifest.Omitted, manifestOmit{Path: cand.rel, Reason: OmitReasonFileCount, Bytes: cand.size})
			continue
		}
		data, err := os.ReadFile(cand.abs)
		if err != nil {
			return bundle{}, fmt.Errorf("reading %s: %w", cand.abs, err)
		}
		data, redacted := redact(data)
		if b.bytes+int64(len(data)) > MaxCommitBytes {
			b.manifest.Omitted = append(b.manifest.Omitted, manifestOmit{Path: cand.rel, Reason: OmitReasonCommitSize, Bytes: cand.size})
			continue
		}
		sum := sha256.Sum256(data)
		digest := hex.EncodeToString(sum[:])
		fmt.Fprintf(hasher, "%s\x00%s\n", cand.rel, digest)
		b.files = append(b.files, File{Path: s.repoPath(c.id, cand.rel), Content: data})
		b.manifest.Files = append(b.manifest.Files, manifestFile{Path: cand.rel, Bytes: len(data), SHA256: digest, Redacted: redacted})
		b.bytes += int64(len(data))
		if redacted {
			b.redacted++
		}
	}
	b.fingerprint = hex.EncodeToString(hasher.Sum(nil))
	b.manifest.Fingerprint = b.fingerprint
	b.manifest.Note = "Mirrored by the hive's Nous artifact sync. Nothing here is promoted automatically; an owner promotes a verdict explicitly from the Experiment Lab."
	return b, nil
}

// redact runs text through pkg/logscrub and reports whether anything
// changed. Binary content (invalid UTF-8 or NUL bytes) is left as is.
func redact(data []byte) ([]byte, bool) {
	if len(data) == 0 || !utf8.Valid(data) || strings.IndexByte(string(data), 0) >= 0 {
		return data, false
	}
	scrubbed := logscrub.ScrubString(string(data))
	if scrubbed == string(data) {
		return data, false
	}
	return []byte(scrubbed), true
}

// repoPath is <prefix>/<campaign>/<rel>.
func (s *Syncer) repoPath(campaignID, rel string) string {
	parts := []string{}
	if s.settings.PathPrefix != "" {
		parts = append(parts, s.settings.PathPrefix)
	}
	parts = append(parts, campaignID, rel)
	return path.Join(parts...)
}

// BranchFor is the PR branch name for a campaign.
func BranchFor(campaignID string) string {
	safe := branchUnsafe.ReplaceAllString(campaignID, "-")
	safe = strings.Trim(safe, "-./")
	if safe == "" {
		safe = "campaign"
	}
	return BranchPrefix + safe
}

// CommitHeadline is the deterministic first line of a mirror commit.
func CommitHeadline(campaignID, status string, files int) string {
	return fmt.Sprintf("nous: mirror campaign %s (%s, %d files)", campaignID, status, files)
}

func (s *Syncer) resolveBase(ctx context.Context) (string, error) {
	if s.settings.Branch != "" {
		return s.settings.Branch, nil
	}
	s.mu.Lock()
	cached := s.defaultBranch
	s.mu.Unlock()
	if cached != "" {
		return cached, nil
	}
	resolved, err := s.forge.DefaultBranch(ctx, s.settings.Repo)
	if err != nil {
		return "", fmt.Errorf("resolving default branch of %s: %w", s.settings.Repo, err)
	}
	resolved = strings.TrimSpace(resolved)
	if resolved == "" {
		return "", fmt.Errorf("resolving default branch of %s: empty", s.settings.Repo)
	}
	s.mu.Lock()
	s.defaultBranch = resolved
	s.status.Branch = resolved
	s.mu.Unlock()
	return resolved, nil
}

// push writes one commit on the campaign's branch and opens (or reuses) the
// PR, then records the result in state and the audit log.
func (s *Syncer) push(ctx context.Context, now time.Time, c campaign, b bundle) error {
	if len(b.files) == 0 {
		return fmt.Errorf("campaign %s has no mirrorable files (%d omitted)", c.id, len(b.manifest.Omitted))
	}
	base, err := s.resolveBase(ctx)
	if err != nil {
		return err
	}
	baseSHA, exists, err := s.forge.BranchHead(ctx, s.settings.Repo, base)
	if err != nil {
		return err
	}
	if !exists {
		return fmt.Errorf("target branch %q does not exist in %s", base, s.settings.Repo)
	}
	head := BranchFor(c.id)
	headSHA, exists, err := s.forge.BranchHead(ctx, s.settings.Repo, head)
	if err != nil {
		return err
	}
	if !exists {
		if err := s.forge.CreateBranch(ctx, s.settings.Repo, head, baseSHA); err != nil {
			return err
		}
		headSHA = baseSHA
	}

	b.manifest.SyncedAt = now.UTC()
	manifestJSON, err := json.MarshalIndent(b.manifest, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding manifest: %w", err)
	}
	files := append(append([]File(nil), b.files...), File{Path: s.repoPath(c.id, ManifestFile), Content: append(manifestJSON, '\n')})

	headline := CommitHeadline(c.id, c.status, len(b.files))
	commitSHA, err := s.forge.CommitFiles(ctx, s.settings.Repo, head, headSHA, headline, commitBody(c, b), files)
	if err != nil {
		return err
	}
	pr, err := s.forge.CreatePullRequest(ctx, s.settings.Repo, head, base, prTitle(c), prBody(c, b, commitSHA))
	if err != nil {
		return fmt.Errorf("commit %s pushed to %s but opening the pull request failed: %w", commitSHA, head, err)
	}

	rec := CampaignRecord{
		Status: c.status, Fingerprint: b.fingerprint, Commit: commitSHA, Branch: head,
		PRNumber: pr.Number, PRURL: pr.URL, Files: len(b.files), Omitted: len(b.manifest.Omitted),
		Redacted: b.redacted, Bytes: b.bytes, SyncedAt: now,
	}
	s.mu.Lock()
	s.state[c.id] = rec
	s.saveStateLocked()
	s.mu.Unlock()
	s.logger.Info("nous artifact mirror: pushed campaign", "campaign", c.id, "status", c.status, "repo", s.settings.Repo, "branch", head, "commit", commitSHA, "pr", pr.URL, "files", len(b.files), "omitted", len(b.manifest.Omitted), "redacted", b.redacted)
	if s.Audit != nil {
		s.Audit(AuditActionPush, fmt.Sprintf("campaign=%s, status=%s, branch=%s, commit=%s, files=%d, omitted=%d, redacted=%d, bytes=%d, pr=%s", c.id, c.status, head, commitSHA, len(b.files), len(b.manifest.Omitted), b.redacted, b.bytes, pr.URL), s.settings.Repo, pr.Number)
	}
	return nil
}

func commitBody(c campaign, b bundle) string {
	return fmt.Sprintf("Hive Nous artifact mirror.\n\nCampaign: %s\nStatus: %s\nFingerprint: %s\nFiles: %d (omitted %d, redacted %d)\n\nNothing is promoted by this commit; see %s for the file list.",
		c.id, c.status, b.fingerprint, len(b.files), len(b.manifest.Omitted), b.redacted, ManifestFile)
}

func prTitle(c campaign) string {
	return fmt.Sprintf("Nous artifacts: %s (%s)", c.id, c.status)
}

func prBody(c campaign, b bundle, commit string) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "Mirror of the Nous campaign `%s` from the hive's durable storage.\n\n", c.id)
	fmt.Fprintf(&sb, "- Status: **%s**\n- Files: %d (omitted %d, redacted %d)\n- Commit: `%s`\n- Fingerprint: `%s`\n\n", c.status, len(b.files), len(b.manifest.Omitted), b.redacted, commit, b.fingerprint)
	if c.status == StatusIncomplete {
		sb.WriteString("This campaign ended without a verdict (crashed, aborted or failed); the folder is mirrored as-is and marked incomplete.\n\n")
	}
	sb.WriteString("Nothing is promoted by this pull request. An owner promotes a verdict explicitly from the Experiment Lab; this repository only keeps the record.\n")
	return sb.String()
}

func summarizeFailures(failed map[string]string) string {
	ids := make([]string, 0, len(failed))
	for id := range failed {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	parts := make([]string, 0, len(ids))
	for _, id := range ids {
		parts = append(parts, id+": "+failed[id])
	}
	return strings.Join(parts, "; ")
}

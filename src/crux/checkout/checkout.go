// Package checkout is the pinned-checkout work service (hivecommons/crux#114
// S1): "a working copy of this repo at this commit, owned by this caller,
// capped, deletable". It builds on sandbox.WorktreeManager for the pinned
// worktree and keeps one JSON record per checkout plus an append-only
// index under Config.Dir, the way sandbox_run.go keeps run records.
//
// Trust is deny-by-default: a repository must match one of the configured
// URL prefixes (the shape of Hive's nousdeps fetch allowlist, as a prefix
// list) or it is refused before any clone is attempted. A commit must be a
// full 40-hex SHA so the pin cannot move under the caller.
//
// The size cap is honest, not pre-emptive: git cannot bound a clone before
// transferring it, so the clone and worktree are measured after they land
// and removed when they exceed the cap (ErrTooLarge). When they cannot be
// measured the checkout is removed and the call fails with ErrUnsupported
// rather than delivering an uncapped copy. The operator's Config.MaxBytes
// is the ceiling a request's max_bytes may not exceed (ErrAboveCeiling)
// and the cap when the request names none.
//
// Private repositories: Config.Token, when set, reaches git only through a
// credential helper that reads it from the child's environment — never on
// the command line, never in a record, never in an error. Git is run with
// GIT_TERMINAL_PROMPT=0 so a missing credential fails instead of hanging.
package checkout

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/hivecommons/hive/crux/logscrub"
	"github.com/hivecommons/hive/crux/sandbox"
)

const (
	// DirName is the directory under the work dir that holds checkouts.
	DirName = "checkouts"
	// IndexName is the append-only create/delete history inside Dir.
	IndexName = "index.jsonl"
	// IDPrefix starts every checkout id.
	IDPrefix = "chk_"
	// DefaultMaxBytes is the server ceiling on a checkout's clone plus
	// worktree, and the cap when a request names none: 5 GiB.
	DefaultMaxBytes int64 = 5 << 30
	// TokenUser is the username the credential helper pairs with
	// Config.Token; GitHub and GitLab accept it for any token.
	TokenUser = "x-access-token"
	// tokenEnvVar carries Config.Token into the git child process for the
	// credential helper to read; it is set on that process only.
	tokenEnvVar = "CRUX_CHECKOUT_GIT_TOKEN"
	// DefaultMaxCheckouts bounds live worktrees under Dir.
	DefaultMaxCheckouts = 64
	// MaxOwnerLen bounds the owner identifier.
	MaxOwnerLen = 64
	// commitHexLen is the length of a full SHA-1 commit id.
	commitHexLen = 40
	// idRandomBytes is the entropy behind an id (12 hex chars).
	idRandomBytes = 6
	// sourceDirName holds the per-(repo, owner) clones worktrees pin from.
	sourceDirName = "src"
	// worktreeStage is the sandbox.WorktreeSpec stage name; a checkout has
	// one generation, so the worktree lands at runs/<id>/pin-1.
	worktreeStage = "pin"
	// recordMode and dirMode keep records readable by the runtime's uid only.
	recordMode = 0o600
	dirMode    = 0o700
)

// Sentinel errors; Service methods wrap them so callers map them to typed
// API codes with errors.Is.
var (
	// ErrBadRequest is a malformed request (short SHA, bad URL, bad owner).
	ErrBadRequest = errors.New("checkout: bad request")
	// ErrForbidden is a repository off the allowlist or a caller that does
	// not own the checkout.
	ErrForbidden = errors.New("checkout: forbidden")
	// ErrNotFound is an unknown checkout id.
	ErrNotFound = errors.New("checkout: not found")
	// ErrInUse is a delete while contained runs still use the checkout.
	ErrInUse = errors.New("checkout: in use")
	// ErrMismatch is a worktree whose HEAD is not the requested commit.
	ErrMismatch = errors.New("checkout: commit mismatch")
	// ErrTooLarge is a clone that exceeded max_bytes once measured.
	ErrTooLarge = errors.New("checkout: exceeds size cap")
	// ErrAboveCeiling is a request whose max_bytes is above the server's
	// Config.MaxBytes; it is also an ErrBadRequest.
	ErrAboveCeiling = errors.New("checkout: max_bytes above the server ceiling")
	// ErrUnsupported is a cap the backend could not verify.
	ErrUnsupported = errors.New("checkout: cannot enforce size cap")
	// ErrGit is a git failure (clone, fetch, worktree).
	ErrGit = errors.New("checkout: git")
)

var (
	commitPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)
	ownerPattern  = regexp.MustCompile(`^[a-z0-9][a-z0-9_.-]*$`)
)

// Runner executes one command in dir and returns its combined output; the
// default runs it with os/exec. Tests substitute failures.
type Runner func(ctx context.Context, dir string, name string, args ...string) ([]byte, error)

// Config is the administrator-owned configuration of the service.
type Config struct {
	// Dir is where clones, worktrees and records live (DefaultDir).
	Dir string
	// AllowedRepos are URL prefixes a requested repo must start with. Empty
	// denies every request.
	AllowedRepos []string
	// MaxCheckouts caps live worktrees; zero means DefaultMaxCheckouts.
	MaxCheckouts int
	// MaxBytes is the ceiling on a request's max_bytes and the cap when a
	// request names none; zero means DefaultMaxBytes.
	MaxBytes int64
	// Token authenticates https clones of private repositories. It is
	// handed to git through a credential helper reading the child's
	// environment and appears in no record, argument or error.
	Token string
	// Git runs git; nil uses os/exec. When Token is set the runner is
	// wrapped so every git invocation carries the credential helper.
	Git Runner
	// Now is the clock; nil uses time.Now.
	Now func() time.Time
	// InUse reports whether contained runs still reference a checkout; the
	// contained-run service wires it. Nil means never in use.
	InUse func(id string) bool
	// DiskUsage measures a tree in bytes; nil walks it.
	DiskUsage func(path string) (int64, error)
}

// DefaultDir is Dir under the runtime's work dir.
func DefaultDir(workDir string) string { return filepath.Join(workDir, DirName) }

// Request is what a caller asks for.
type Request struct {
	Repo     string
	Commit   string
	Owner    string
	Branches *bool
	MaxBytes int64
	TTL      time.Duration
}

// Record is one checkout as stored under Dir/<id>.json. It never carries
// environment or credentials: the repo URL is refused if it embeds any.
type Record struct {
	ID        string     `json:"id"`
	Path      string     `json:"path"`
	Repo      string     `json:"repo"`
	Commit    string     `json:"commit"`
	Owner     string     `json:"owner"`
	Branches  bool       `json:"branches"`
	MaxBytes  int64      `json:"max_bytes"`
	Bytes     int64      `json:"bytes"`
	TTL       string     `json:"ttl,omitempty"`
	CreatedAt time.Time  `json:"created_at"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
	// Source is the shared clone the worktree pins from.
	Source string `json:"source"`
}

// Service creates, lists and deletes checkouts.
type Service struct {
	cfg   Config
	trees sandbox.WorktreeManager
	store *store
}

// New validates cfg and returns a Service. Dir is created on first use.
func New(cfg Config) (*Service, error) {
	if strings.TrimSpace(cfg.Dir) == "" {
		return nil, fmt.Errorf("%w: dir is required", ErrBadRequest)
	}
	for i, p := range cfg.AllowedRepos {
		p = strings.TrimSpace(p)
		if _, err := parseRepo(p); err != nil {
			return nil, fmt.Errorf("allowlist entry %q: %w", p, err)
		}
		cfg.AllowedRepos[i] = p
	}
	if cfg.MaxCheckouts <= 0 {
		cfg.MaxCheckouts = DefaultMaxCheckouts
	}
	if cfg.MaxBytes < 0 {
		return nil, fmt.Errorf("%w: max_bytes must not be negative", ErrBadRequest)
	}
	if cfg.MaxBytes == 0 {
		cfg.MaxBytes = DefaultMaxBytes
	}
	cfg.Token = strings.TrimSpace(cfg.Token)
	if cfg.Git == nil {
		cfg.Git = execRunner(cfg.Token)
	}
	if cfg.Token != "" {
		cfg.Git = withCredentialHelper(cfg.Git)
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.DiskUsage == nil {
		cfg.DiskUsage = diskUsage
	}
	return &Service{cfg: cfg, trees: sandbox.WorktreeManager{Runner: cfg.Git}, store: &store{dir: cfg.Dir}}, nil
}

// InUse reports whether contained runs reference id (Config.InUse).
func (s *Service) InUse(id string) bool {
	return s.cfg.InUse != nil && s.cfg.InUse(id)
}

// Allowed reports whether repo is on the allowlist, naming the rule that
// admitted it or an ErrForbidden explaining why none did.
func (s *Service) Allowed(repo string) (rule string, err error) {
	if len(s.cfg.AllowedRepos) == 0 {
		return "", fmt.Errorf("%w: repository %q: the checkout allowlist is empty", ErrForbidden, repo)
	}
	for _, p := range s.cfg.AllowedRepos {
		if prefixMatches(repo, p) {
			return p, nil
		}
	}
	return "", fmt.Errorf("%w: repository %q matches none of the allowed prefixes %s", ErrForbidden, repo, strings.Join(s.cfg.AllowedRepos, ", "))
}

// ValidateRepoURL reports whether raw is a repository URL this package would
// accept: https:// or file://, no credentials, query or fragment, and a path
// already in canonical form (see parseRepo). The error wraps ErrBadRequest.
// Other services that gate a clone on a URL-prefix allowlist (the runs
// package's ad-hoc worktree form) call it so the string they compare is the
// string git will fetch.
func ValidateRepoURL(raw string) error {
	_, err := parseRepo(raw)
	return err
}

// PrefixMatches is prefixMatches for other allowlist holders: it reports
// whether repo falls under the allowlist prefix p on a segment boundary.
func PrefixMatches(repo, p string) bool { return prefixMatches(repo, p) }

// prefixMatches reports whether repo falls under the allowlist prefix p.
// A prefix that ends in "/" is a directory and admits everything beneath
// it; any other prefix must be followed by a segment boundary ("/", the
// ".git" suffix, or the end of the URL), so "https://h/org" does not admit
// "https://h/org-evil/x" or "https://h.evil/x".
func prefixMatches(repo, p string) bool {
	rest, ok := strings.CutPrefix(repo, p)
	if !ok {
		return false
	}
	if strings.HasSuffix(p, "/") {
		return true
	}
	return rest == "" || rest == ".git" || strings.HasPrefix(rest, "/")
}

// Create provisions a checkout for req, recorded as owned by req.Owner.
func (s *Service) Create(ctx context.Context, req Request) (Record, error) {
	rec, err := s.validate(req)
	if err != nil {
		return Record{}, err
	}
	if _, err := s.Allowed(rec.Repo); err != nil {
		return Record{}, err
	}
	if err := os.MkdirAll(s.cfg.Dir, dirMode); err != nil {
		return Record{}, err
	}
	rec.Source = s.sourceDir(rec.Repo, rec.Owner)
	if err := s.ensureSource(ctx, rec); err != nil {
		return Record{}, err
	}
	tree, err := s.trees.Add(ctx, sandbox.WorktreeSpec{
		SourceRepo: rec.Source, WorkspaceDir: s.cfg.Dir, RunKey: rec.ID, Stage: worktreeStage, Generation: 1,
		TargetRef: rec.Commit, MaxWorktrees: s.cfg.MaxCheckouts,
	})
	if err != nil {
		return Record{}, fmt.Errorf("%w: %v", ErrGit, err)
	}
	rec.Path = tree.Path
	if tree.SHA != rec.Commit {
		_ = s.trees.Remove(ctx, rec.Source, rec.Path)
		return Record{}, fmt.Errorf("%w: requested %s, worktree is at %s", ErrMismatch, rec.Commit, tree.SHA)
	}
	if err := s.enforceCap(&rec); err != nil {
		_ = s.trees.Remove(ctx, rec.Source, rec.Path)
		return Record{}, err
	}
	if err := s.store.put(rec); err != nil {
		_ = s.trees.Remove(ctx, rec.Source, rec.Path)
		return Record{}, err
	}
	return rec, s.store.appendIndex(indexEntry{Op: "create", ID: rec.ID, Owner: rec.Owner, Repo: rec.Repo, Commit: rec.Commit, At: rec.CreatedAt})
}

// Get returns the record for id.
func (s *Service) Get(id string) (Record, error) { return s.store.get(id) }

// List returns every record, or only owner's when owner is non-empty,
// ordered by id.
func (s *Service) List(owner string) ([]Record, error) {
	all, err := s.store.list()
	if err != nil {
		return nil, err
	}
	if owner == "" {
		return all, nil
	}
	out := all[:0]
	for _, r := range all {
		if r.Owner == owner {
			out = append(out, r)
		}
	}
	return out, nil
}

// Delete removes the worktree and the record. by is who asked, for the
// index; ownership is the caller's decision (see CanDelete).
func (s *Service) Delete(ctx context.Context, id, by string) error {
	rec, err := s.store.get(id)
	if err != nil {
		return err
	}
	if s.InUse(id) {
		return fmt.Errorf("%w: checkout %s has live contained runs", ErrInUse, id)
	}
	if err := s.trees.Remove(ctx, rec.Source, rec.Path); err != nil {
		return fmt.Errorf("%w: remove worktree: %v", ErrGit, err)
	}
	if err := s.store.remove(id); err != nil {
		return err
	}
	return s.store.appendIndex(indexEntry{Op: "delete", ID: id, Owner: rec.Owner, Repo: rec.Repo, Commit: rec.Commit, At: s.cfg.Now().UTC(), By: by})
}

// CanDelete is the ownership rule: the owner or an operator.
func CanDelete(rec Record, caller string, operator bool) bool {
	return operator || (caller != "" && caller == rec.Owner)
}

// CanUse is the same rule for running against a checkout (S2): only the
// owner or an operator may point a contained run at it.
func CanUse(rec Record, caller string, operator bool) bool { return CanDelete(rec, caller, operator) }

// validate checks req and returns the record it will produce.
func (s *Service) validate(req Request) (Record, error) {
	repo := strings.TrimSpace(req.Repo)
	if _, err := parseRepo(repo); err != nil {
		return Record{}, err
	}
	commit := strings.ToLower(strings.TrimSpace(req.Commit))
	if !commitPattern.MatchString(commit) {
		return Record{}, fmt.Errorf("%w: commit must be a full %d-hex SHA, not a tag or branch (got %q)", ErrBadRequest, commitHexLen, req.Commit)
	}
	owner := strings.TrimSpace(req.Owner)
	if owner == "" || len(owner) > MaxOwnerLen || !ownerPattern.MatchString(owner) {
		return Record{}, fmt.Errorf("%w: owner must be a plain identifier of at most %d characters (got %q)", ErrBadRequest, MaxOwnerLen, req.Owner)
	}
	if req.MaxBytes < 0 || req.TTL < 0 {
		return Record{}, fmt.Errorf("%w: max_bytes and ttl must not be negative", ErrBadRequest)
	}
	if req.MaxBytes > s.cfg.MaxBytes {
		return Record{}, fmt.Errorf("%w: %w: request asks for %d bytes, this server allows at most %d", ErrBadRequest, ErrAboveCeiling, req.MaxBytes, s.cfg.MaxBytes)
	}
	id, err := newID()
	if err != nil {
		return Record{}, err
	}
	now := s.cfg.Now().UTC()
	rec := Record{ID: id, Repo: repo, Commit: commit, Owner: owner, Branches: req.Branches == nil || *req.Branches, MaxBytes: req.MaxBytes, CreatedAt: now}
	if rec.MaxBytes == 0 {
		rec.MaxBytes = s.cfg.MaxBytes
	}
	if req.TTL > 0 {
		rec.TTL = req.TTL.String()
		exp := now.Add(req.TTL)
		rec.ExpiresAt = &exp
	}
	return rec, nil
}

// parseRepo accepts an https:// or file:// URL without credentials, query or
// fragment, so a token can never ride into a record. The path must already
// be in canonical form — no ".", ".." or empty segments and no
// percent-encoding — because Allowed compares the string the caller sent
// while git (via libcurl, or literally for file://) resolves dot segments:
// "https://h/allowed/../other/repo" would pass a prefix check for
// "https://h/allowed/" and clone other/repo.
func parseRepo(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "https" && u.Scheme != "file") || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, fmt.Errorf("%w: repo must be an https:// or file:// URL without credentials, query or fragment (got %q)", ErrBadRequest, raw)
	}
	if (u.Scheme == "https" && u.Host == "") || (u.Scheme == "file" && u.Path == "") {
		return nil, fmt.Errorf("%w: repo %q names no host or path", ErrBadRequest, raw)
	}
	if u.RawPath != "" || !canonicalPath(u.Path) {
		return nil, fmt.Errorf("%w: repo path must be canonical — no \".\", \"..\" or empty segments and no percent-encoding (got %q)", ErrBadRequest, raw)
	}
	return u, nil
}

// canonicalPath reports whether p has no ".", ".." or empty segments. A
// single trailing "/" is allowed.
func canonicalPath(p string) bool {
	p = strings.TrimSuffix(p, "/")
	if p == "" {
		return true
	}
	for _, seg := range strings.Split(strings.TrimPrefix(p, "/"), "/") {
		if seg == "" || seg == "." || seg == ".." {
			return false
		}
	}
	return true
}

// sourceDir is the shared clone for (repo, owner).
func (s *Service) sourceDir(repo, owner string) string {
	sum := sha256.Sum256([]byte(repo))
	return filepath.Join(s.cfg.Dir, sourceDirName, owner+"-"+hex.EncodeToString(sum[:6]))
}

// ensureSource clones rec.Repo into rec.Source once and makes sure
// rec.Commit is present, fetching it when a reused clone lacks it.
func (s *Service) ensureSource(ctx context.Context, rec Record) error {
	if _, err := os.Stat(filepath.Join(rec.Source, ".git")); err != nil {
		if err := os.MkdirAll(filepath.Dir(rec.Source), dirMode); err != nil {
			return err
		}
		args := []string{"clone", "--no-checkout"}
		if !rec.Branches {
			args = append(args, "--single-branch")
		}
		args = append(args, "--", rec.Repo, rec.Source)
		if out, err := s.cfg.Git(ctx, s.cfg.Dir, "git", args...); err != nil {
			_ = os.RemoveAll(rec.Source)
			return fmt.Errorf("%w: clone: %v: %s", ErrGit, err, gitOutput(out))
		}
	}
	if _, err := s.cfg.Git(ctx, rec.Source, "git", "cat-file", "-e", rec.Commit+"^{commit}"); err == nil {
		return nil
	}
	if out, err := s.cfg.Git(ctx, rec.Source, "git", "fetch", "--quiet", "origin", rec.Commit); err != nil {
		return fmt.Errorf("%w: commit %s is not in %s and could not be fetched: %s", ErrBadRequest, rec.Commit, rec.Repo, gitOutput(out))
	}
	if _, err := s.cfg.Git(ctx, rec.Source, "git", "cat-file", "-e", rec.Commit+"^{commit}"); err != nil {
		return fmt.Errorf("%w: commit %s is not in %s", ErrBadRequest, rec.Commit, rec.Repo)
	}
	return nil
}

// enforceCap measures the clone and the worktree after the fact and refuses
// the checkout when they exceed rec.MaxBytes or cannot be measured.
func (s *Service) enforceCap(rec *Record) error {
	src, err := s.cfg.DiskUsage(rec.Source)
	if err != nil {
		return fmt.Errorf("%w: measuring %s: %v", ErrUnsupported, rec.Source, err)
	}
	tree, err := s.cfg.DiskUsage(rec.Path)
	if err != nil {
		return fmt.Errorf("%w: measuring %s: %v", ErrUnsupported, rec.Path, err)
	}
	rec.Bytes = src + tree
	if rec.Bytes > rec.MaxBytes {
		return fmt.Errorf("%w: clone and worktree measure %d bytes, cap is %d (the cap is checked after cloning; git cannot bound a clone in advance)", ErrTooLarge, rec.Bytes, rec.MaxBytes)
	}
	return nil
}

func newID() (string, error) {
	b := make([]byte, idRandomBytes)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return IDPrefix + hex.EncodeToString(b), nil
}

// ValidID reports whether id has the shape newID produces, so a path value
// can never escape Dir.
func ValidID(id string) bool {
	rest, ok := strings.CutPrefix(id, IDPrefix)
	if !ok || len(rest) != 2*idRandomBytes {
		return false
	}
	_, err := hex.DecodeString(rest)
	return err == nil
}

// execRunner is the os/exec Runner. Git never prompts, and token (when
// set) is in the child's environment for the credential helper alone.
func execRunner(token string) Runner {
	return func(ctx context.Context, dir string, name string, args ...string) ([]byte, error) {
		cmd := exec.CommandContext(ctx, name, args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
		if token != "" {
			cmd.Env = append(cmd.Env, tokenEnvVar+"="+token)
		}
		return cmd.CombinedOutput()
	}
}

// credentialHelper is the inline helper git runs on an https challenge: it
// answers with TokenUser and the token from the child's environment, so
// the secret is never an argument. The leading "!" marks a shell snippet.
const credentialHelper = "!f() { echo username=" + TokenUser + "; echo \"password=$" + tokenEnvVar + "\"; }; f"

// withCredentialHelper wraps inner so every git invocation first clears any
// configured helpers (a host-wide one could leak the operator's own
// credential into a caller's clone) and then installs credentialHelper.
func withCredentialHelper(inner Runner) Runner {
	return func(ctx context.Context, dir string, name string, args ...string) ([]byte, error) {
		if name == "git" {
			args = append([]string{"-c", "credential.helper=", "-c", "credential.helper=" + credentialHelper}, args...)
		}
		return inner(ctx, dir, name, args...)
	}
}

// gitOutput is git's combined output as it may appear in an error: trimmed
// and scrubbed, in case a server ever echoes a credential back.
func gitOutput(out []byte) string {
	return logscrub.ScrubString(strings.TrimSpace(string(out)))
}

// diskUsage sums regular file sizes under path.
func diskUsage(path string) (int64, error) {
	var total int64
	err := filepath.WalkDir(path, func(_ string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type().IsRegular() {
			info, err := d.Info()
			if err != nil {
				return err
			}
			total += info.Size()
		}
		return nil
	})
	return total, err
}

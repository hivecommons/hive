package nousdeps

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/nousstore"
)

// Fetch limits. Named so a change is a one-line, reviewable diff.
const (
	// DefaultFetchTimeout bounds the whole setup step.
	DefaultFetchTimeout = 10 * time.Minute
	// DefaultMaxFileBytes caps one file download; traces and model configs
	// are far smaller, and anything bigger belongs on an external host.
	DefaultMaxFileBytes int64 = 2 << 30
	// maxRedirects bounds a file download's redirect chain; every hop is
	// checked against the allowlist.
	maxRedirects = 5
	// gitFetchDepth: a pinned commit needs no history.
	gitFetchDepth = "1"

	repoCacheSubdir = "repos"
	fileCacheSubdir = "files"
	tmpSuffix       = ".tmp"
	dirPerm         = 0o755
	filePerm        = 0o644
)

// Runner executes a command in dir and returns its combined output. It is
// the seam tests use to replace git and the toolchain probes.
type Runner func(ctx context.Context, dir, name string, args ...string) (string, error)

// ExecRunner is the production Runner: os/exec with the current environment.
func ExecRunner(ctx context.Context, dir, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out)), nil
}

// Options drives Setup. Zero values pick production defaults.
type Options struct {
	// CampaignPath is the campaign.yaml to read dependencies from.
	CampaignPath string
	// RunID names the campaign run; the record is written under
	// <Root>/<RunID>/. Empty → nousstore.GovernorSubdir.
	RunID string
	// Root is the durable Nous root. Empty → nousstore.DefaultRoot.
	Root string
	// CacheDir is where fetched dependencies are kept. Empty → EnvCacheDir
	// if set, else <Root>/CacheSubdir.
	CacheDir string
	// Allowlist is the resolved fetch allowlist. Nil denies everything;
	// callers resolve the default through config (AllowlistFromEnv).
	Allowlist Allowlist
	// NousDir is the Nous install whose version is recorded. Empty →
	// EnvNousDir if set, else DefaultNousDir.
	NousDir string
	// Run executes git and the toolchain probes. Nil → ExecRunner.
	Run Runner
	// HTTP downloads files. Nil → a client with DefaultFetchTimeout.
	HTTP *http.Client
	// MaxFileBytes caps one download. Zero → DefaultMaxFileBytes.
	MaxFileBytes int64
	// Now is the record clock. Nil → time.Now.
	Now func() time.Time
	// Getenv reads the environment. Nil → os.Getenv.
	Getenv func(string) string
	// Logf receives progress lines. Nil discards them.
	Logf func(format string, args ...any)
}

// Resolved is one dependency as actually fetched.
type Resolved struct {
	Name   string `json:"name"`
	Role   string `json:"role"`
	Kind   string `json:"kind"`
	Source string `json:"source"`
	// Pin is the declared pin (commit or sha256).
	Pin string `json:"pin"`
	// Resolved is what was verified on disk: the checked-out commit, or
	// the computed sha256. It always equals Pin in a written record.
	Resolved string `json:"resolved"`
	Path     string `json:"path"`
	Cached   bool   `json:"cached"`
	Bytes    int64  `json:"bytes,omitempty"`
}

// Record is RecordFile: everything needed to reproduce the campaign's inputs.
type Record struct {
	Schema         int               `json:"schema"`
	Campaign       string            `json:"campaign,omitempty"`
	CampaignPath   string            `json:"campaign_path"`
	RunID          string            `json:"run_id"`
	RecordedAt     string            `json:"recorded_at"`
	FetchAllowlist []string          `json:"fetch_allowlist"`
	Dependencies   []Resolved        `json:"dependencies"`
	Toolchains     map[string]string `json:"toolchains"`
	// RecordPath is where this record was written; informational.
	RecordPath string `json:"record_path"`
}

// Toolchain keys in Record.Toolchains.
const (
	ToolchainNous   = "nous_commit"
	ToolchainPython = "python"
	ToolchainGo     = "go"
	ToolchainGit    = "git"
	// toolchainUnavailable is recorded when a probe fails, so the record
	// says "we looked" rather than omitting the key.
	toolchainUnavailable = "unavailable"
)

func (o *Options) defaults() {
	if o.Getenv == nil {
		o.Getenv = os.Getenv
	}
	if o.Root == "" {
		o.Root = nousstore.Resolve("", o.Getenv).Root
	}
	if o.RunID == "" {
		o.RunID = nousstore.GovernorSubdir
	}
	if o.CacheDir == "" {
		if c := strings.TrimSpace(o.Getenv(EnvCacheDir)); c != "" {
			o.CacheDir = filepath.Join(c, filepath.Base(CacheSubdir))
		} else {
			o.CacheDir = filepath.Join(o.Root, CacheSubdir)
		}
	}
	if o.NousDir == "" {
		if d := strings.TrimSpace(o.Getenv(EnvNousDir)); d != "" {
			o.NousDir = d
		} else {
			o.NousDir = DefaultNousDir
		}
	}
	if o.Run == nil {
		o.Run = ExecRunner
	}
	if o.MaxFileBytes <= 0 {
		o.MaxFileBytes = DefaultMaxFileBytes
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Logf == nil {
		o.Logf = func(string, ...any) {}
	}
	if o.HTTP == nil {
		o.HTTP = &http.Client{Timeout: DefaultFetchTimeout}
	}
}

// RecordPath is where Setup writes RecordFile for a run.
func RecordPath(root, runID string) string {
	return filepath.Join(root, runID, RecordFile)
}

// EnvPath is where Setup writes EnvFile for a run.
func EnvPath(root, runID string) string {
	return filepath.Join(root, runID, EnvFile)
}

// Setup is the setup step: load and validate the campaign's dependencies,
// refuse any outside the allowlist BEFORE fetching anything, fetch and
// verify each one, record toolchain versions, and write RecordFile and
// EnvFile. A failure leaves no record so the runner never starts a campaign
// on half-verified inputs.
func Setup(ctx context.Context, opts Options) (Record, error) {
	opts.defaults()
	deps, err := Load(opts.CampaignPath)
	if err != nil {
		return Record{}, err
	}
	// Allowlist check up front: one bad declaration fails the campaign
	// before any bytes move.
	for _, d := range deps.Dependencies {
		if _, err := opts.Allowlist.CheckURL(d.SourceURL()); err != nil {
			return Record{}, fmt.Errorf("dependency %q: %w", d.Name, err)
		}
	}
	if err := os.MkdirAll(opts.CacheDir, dirPerm); err != nil {
		return Record{}, fmt.Errorf("nousdeps: cache dir: %w", err)
	}
	rec := Record{
		Schema:         RecordSchema,
		Campaign:       deps.Name,
		CampaignPath:   opts.CampaignPath,
		RunID:          opts.RunID,
		RecordedAt:     opts.Now().UTC().Format(time.RFC3339),
		FetchAllowlist: append([]string{}, opts.Allowlist...),
		Dependencies:   make([]Resolved, 0, len(deps.Dependencies)),
	}
	if rec.FetchAllowlist == nil {
		rec.FetchAllowlist = []string{}
	}
	for _, d := range deps.Dependencies {
		var r Resolved
		var err error
		switch d.Kind() {
		case KindRepo:
			r, err = fetchRepo(ctx, opts, d)
		default:
			r, err = fetchFile(ctx, opts, d)
		}
		if err != nil {
			return Record{}, fmt.Errorf("dependency %q: %w", d.Name, err)
		}
		opts.Logf("dependency %s (%s, %s): %s → %s%s", r.Name, r.Kind, r.Role, r.Pin, r.Path, cachedSuffix(r.Cached))
		rec.Dependencies = append(rec.Dependencies, r)
	}
	rec.Toolchains = probeToolchains(ctx, opts)
	rec.RecordPath = RecordPath(opts.Root, opts.RunID)
	if err := writeRecord(opts, rec); err != nil {
		return Record{}, err
	}
	return rec, nil
}

func cachedSuffix(cached bool) string {
	if cached {
		return " (cached)"
	}
	return ""
}

// repoCachePath is <cache>/repos/<host>/<path…>/<commit>, so the same repo
// at two commits never shares a checkout and a campaign can be re-run
// against exactly what it recorded.
func repoCachePath(cacheDir string, u *url.URL, commit string) string {
	parts := []string{cacheDir, repoCacheSubdir, u.Hostname()}
	for _, seg := range strings.Split(strings.Trim(u.Path, "/"), "/") {
		if seg != "" {
			parts = append(parts, strings.TrimSuffix(seg, ".git"))
		}
	}
	parts = append(parts, commit)
	return filepath.Join(parts...)
}

func fetchRepo(ctx context.Context, opts Options, d Dependency) (Resolved, error) {
	u, err := opts.Allowlist.CheckURL(d.SourceURL())
	if err != nil {
		return Resolved{}, err
	}
	commit := d.Pin()
	dest := repoCachePath(opts.CacheDir, u, commit)
	r := Resolved{Name: d.Name, Role: d.Role, Kind: KindRepo, Source: u.String(), Pin: commit, Path: dest}
	if head, err := opts.Run(ctx, dest, "git", "rev-parse", "HEAD"); err == nil && head == commit {
		r.Resolved = head
		r.Cached = true
		return r, nil
	}
	// Anything else at dest is stale or half-fetched; start clean.
	if err := os.RemoveAll(dest); err != nil {
		return Resolved{}, fmt.Errorf("clear stale checkout: %w", err)
	}
	if err := os.MkdirAll(dest, dirPerm); err != nil {
		return Resolved{}, fmt.Errorf("create checkout dir: %w", err)
	}
	steps := [][]string{
		{"git", "init", "--quiet"},
		{"git", "remote", "add", "origin", u.String()},
		{"git", "fetch", "--quiet", "--depth", gitFetchDepth, "origin", commit},
		{"git", "checkout", "--quiet", "--detach", "FETCH_HEAD"},
	}
	for _, step := range steps {
		if _, err := opts.Run(ctx, dest, step[0], step[1:]...); err != nil {
			_ = os.RemoveAll(dest)
			return Resolved{}, err
		}
	}
	head, err := opts.Run(ctx, dest, "git", "rev-parse", "HEAD")
	if err != nil {
		_ = os.RemoveAll(dest)
		return Resolved{}, err
	}
	if head != commit {
		_ = os.RemoveAll(dest)
		return Resolved{}, fmt.Errorf("%w: wanted commit %s, checked out %s", ErrPinMismatch, commit, head)
	}
	r.Resolved = head
	return r, nil
}

// fileCachePath is <cache>/files/<sha256>/<basename>: content-addressed, so
// the cache can be trusted by hash alone.
func fileCachePath(cacheDir string, u *url.URL, sum string) string {
	base := path.Base(u.Path)
	if base == "" || base == "/" || base == "." {
		base = "file"
	}
	return filepath.Join(cacheDir, fileCacheSubdir, sum, base)
}

func fetchFile(ctx context.Context, opts Options, d Dependency) (Resolved, error) {
	u, err := opts.Allowlist.CheckURL(d.SourceURL())
	if err != nil {
		return Resolved{}, err
	}
	want := d.Pin()
	dest := fileCachePath(opts.CacheDir, u, want)
	r := Resolved{Name: d.Name, Role: d.Role, Kind: KindFile, Source: u.String(), Pin: want, Path: dest}
	if sum, n, err := hashFile(dest); err == nil && sum == want {
		r.Resolved, r.Bytes, r.Cached = sum, n, true
		return r, nil
	}
	if err := os.MkdirAll(filepath.Dir(dest), dirPerm); err != nil {
		return Resolved{}, fmt.Errorf("create cache dir: %w", err)
	}
	sum, n, err := download(ctx, opts, u, dest+tmpSuffix)
	if err != nil {
		_ = os.Remove(dest + tmpSuffix)
		return Resolved{}, err
	}
	if sum != want {
		_ = os.Remove(dest + tmpSuffix)
		return Resolved{}, fmt.Errorf("%w: wanted sha256 %s, downloaded %s", ErrPinMismatch, want, sum)
	}
	if err := os.Rename(dest+tmpSuffix, dest); err != nil {
		_ = os.Remove(dest + tmpSuffix)
		return Resolved{}, fmt.Errorf("install file: %w", err)
	}
	r.Resolved, r.Bytes = sum, n
	return r, nil
}

// download GETs u into tmp, enforcing the allowlist on every redirect hop
// and the size cap on the body, and returns the body's sha256 and length.
func download(ctx context.Context, opts Options, u *url.URL, tmp string) (string, int64, error) {
	client := *opts.HTTP
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= maxRedirects {
			return fmt.Errorf("stopped after %d redirects", maxRedirects)
		}
		if _, err := opts.Allowlist.CheckURL(req.URL.String()); err != nil {
			return fmt.Errorf("redirect to %s refused: %w", req.URL.Redacted(), err)
		}
		return nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return "", 0, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", 0, fmt.Errorf("fetch %s: %w", u.Redacted(), err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", 0, fmt.Errorf("fetch %s: HTTP %d", u.Redacted(), resp.StatusCode)
	}
	if resp.ContentLength > opts.MaxFileBytes {
		return "", 0, fmt.Errorf("fetch %s: %d bytes exceeds the %d-byte limit", u.Redacted(), resp.ContentLength, opts.MaxFileBytes)
	}
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, filePerm)
	if err != nil {
		return "", 0, err
	}
	h := sha256.New()
	// +1 so a body exactly at the limit passes and one byte over is caught.
	n, err := io.Copy(io.MultiWriter(f, h), io.LimitReader(resp.Body, opts.MaxFileBytes+1))
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return "", 0, fmt.Errorf("fetch %s: %w", u.Redacted(), err)
	}
	if n > opts.MaxFileBytes {
		return "", 0, fmt.Errorf("fetch %s: body exceeds the %d-byte limit", u.Redacted(), opts.MaxFileBytes)
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

func hashFile(p string) (string, int64, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

// probeToolchains records the versions a verdict depends on: the Nous
// commit actually installed, and the interpreters the campaign runs with.
// A probe that fails records toolchainUnavailable rather than aborting —
// the record is still useful, and a missing Go toolchain is only a problem
// for campaigns that need one.
func probeToolchains(ctx context.Context, opts Options) map[string]string {
	out := map[string]string{}
	probe := func(key, dir, name string, args ...string) {
		v, err := opts.Run(ctx, dir, name, args...)
		if err != nil || v == "" {
			out[key] = toolchainUnavailable
			return
		}
		out[key] = v
	}
	probe(ToolchainNous, opts.NousDir, "git", "-C", opts.NousDir, "rev-parse", "HEAD")
	probe(ToolchainPython, "", filepath.Join(opts.NousDir, "venv", "bin", "python3"), "--version")
	if out[ToolchainPython] == toolchainUnavailable {
		probe(ToolchainPython, "", "python3", "--version")
	}
	probe(ToolchainGo, "", "go", "version")
	probe(ToolchainGit, "", "git", "--version")
	return out
}

// writeRecord writes RecordFile and EnvFile atomically under <root>/<run-id>/.
func writeRecord(opts Options, rec Record) error {
	dir := filepath.Join(opts.Root, opts.RunID)
	if err := os.MkdirAll(dir, dirPerm); err != nil {
		return fmt.Errorf("nousdeps: campaign dir: %w", err)
	}
	data, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return err
	}
	if err := writeAtomic(RecordPath(opts.Root, opts.RunID), append(data, '\n')); err != nil {
		return fmt.Errorf("nousdeps: write %s: %w", RecordFile, err)
	}
	if err := writeAtomic(EnvPath(opts.Root, opts.RunID), []byte(rec.EnvFileContent())); err != nil {
		return fmt.Errorf("nousdeps: write %s: %w", EnvFile, err)
	}
	return nil
}

// EnvFileContent renders the shell-sourceable companion (EnvFile): one NOUS_DEP_<NAME> per
// dependency plus NOUS_TARGET_REPO_PATH for the target, sorted for
// determinism. Values are single-quoted; a path containing a quote is
// escaped the POSIX way.
func (r Record) EnvFileContent() string {
	lines := []string{"# Generated by `hive nous-setup` — pinned campaign dependencies (RFC #11585 part 7). Do not edit."}
	vars := map[string]string{}
	for _, d := range r.Dependencies {
		vars[Dependency{Name: d.Name}.EnvName()] = d.Path
		if d.Role == RoleTarget {
			vars[EnvTargetRepoPath] = d.Path
		}
	}
	keys := make([]string, 0, len(vars))
	for k := range vars {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		lines = append(lines, k+"='"+strings.ReplaceAll(vars[k], "'", `'\''`)+"'")
	}
	return strings.Join(lines, "\n") + "\n"
}

// ReadRecord loads a previously written RecordFile. A missing file is
// reported as fs.ErrNotExist via os.ReadFile.
func ReadRecord(root, runID string) (Record, error) {
	data, err := os.ReadFile(RecordPath(root, runID))
	if err != nil {
		return Record{}, err
	}
	var rec Record
	if err := json.Unmarshal(data, &rec); err != nil {
		return Record{}, fmt.Errorf("nousdeps: parse %s: %w", RecordFile, err)
	}
	return rec, nil
}

func writeAtomic(p string, data []byte) error {
	tmp := p + tmpSuffix
	if err := os.WriteFile(tmp, data, filePerm); err != nil {
		return err
	}
	if err := os.Rename(tmp, p); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

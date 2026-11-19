package proxy

import (
	"bufio"
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/hivecommons/hive/crux/agent"
)

// Reference repositories (hivecommons/hive#18015).
//
// A reference repo is one the hive's agents may STUDY without managing it:
// project.reference_repos. Reads — git clone/fetch and GitHub GETs — pass, and
// pass WITHOUT the hive's credential: the agent's injected token belongs to the
// hive's own installation and is not valid for a foreign repo, so it is
// stripped and the request goes out as the public internet would send it.
//
// Writes are refused unless the entry opted in, and even then only to the
// narrow surface an assessor needs: open an issue, open a PR from a topic
// branch, comment on the hive's OWN issue or PR. Merging, pushing the default
// branch, labels/assignees/closing on anyone's items and reviews are never
// allowed — there is no mode, capability or relay that widens this. The write
// credential is the policy's own Token source (the hive owner's connected
// GitHub account, or an operator-supplied file), never the hive's token.
//
// A PR needs a branch the credential can push, and the owner cannot push to
// a repo they do not own, so a prs-tier entry also allows the fork path
// (hivecommons/hive#18198): fork the reference repo into the owner's account
// (POST /repos/{o}/{r}/forks), push topic branches to that fork
// ({login}/{name}, default branch still protected), and open the PR against
// the reference repo with head {login}:{branch}. Nothing else is allowed on
// the fork.

// DenyReasonReferenceRepo is the 403 reason code for a refused reference-repo
// write (see denial_message.go for the vocabulary).
const DenyReasonReferenceRepo = "reference_repo"

// ReferenceRepoPolicy is the proxy's view of one project.reference_repos entry.
type ReferenceRepoPolicy struct {
	// Repo is the "owner/name" slug.
	Repo string
	// Issues and PRs are the write opt-ins.
	Issues, PRs bool
	// Token yields the write credential for Repo. nil for a read-only entry.
	Token func() (string, bool)
	// Login is the GitHub login the Token acts as (the hive owner's login, or
	// the write_token_file's user); own-item checks compare an item's author
	// against it, and Login/<name> is the fork a PR is pushed through.
	Login string
	// Fork is set only on the copy referenceRepoFor returns for a request
	// bound to Login's fork of Repo (hivecommons/hive#18198): the fork's own
	// "login/name" slug. The installed policy never carries it.
	Fork string
}

// target is the repository a request under this policy actually names: the
// fork when the request is bound to it, else the reference repo itself.
func (r ReferenceRepoPolicy) target() string {
	if r.Fork != "" {
		return r.Fork
	}
	return r.Repo
}

// forkSlug is the slug of Login's fork of Repo ("" when either half is
// unknown). GitHub forks keep the upstream name unless renamed; a renamed
// fork is not recognised and its pushes are refused like any other repo.
//
// An App bot login ("<app>[bot]", project.remote_write_identity bot) owns no
// repositories, so a bot-identity policy has no fork: its topic branches go
// to Repo itself, under the same default-branch guard.
func (r ReferenceRepoPolicy) forkSlug() string {
	if r.Login == "" || strings.HasSuffix(r.Login, "[bot]") {
		return ""
	}
	_, name, ok := strings.Cut(r.Repo, "/")
	if !ok || name == "" {
		return ""
	}
	return r.Login + "/" + name
}

func (r ReferenceRepoPolicy) readOnly() bool { return !r.Issues && !r.PRs }

func (r ReferenceRepoPolicy) tier() string {
	switch {
	case r.Issues && r.PRs:
		return "issues+prs"
	case r.Issues:
		return "issues"
	case r.PRs:
		return "prs"
	}
	return "read-only"
}

// referenceDecision is the outcome of the path-only part of the write check.
type referenceDecision int

const (
	referenceNotReference referenceDecision = iota // not a reference repo, or a read: nothing to do
	referenceAllow
	referenceDeny
	referenceOwnItem // allowed only if item #N was authored by the policy's Login
	referencePush    // git push: allowed only when no ref targets the default branch
)

var (
	refIssuesPath        = regexp.MustCompile(`^/repos/[^/]+/[^/]+/issues$`)
	refPullsPath         = regexp.MustCompile(`^/repos/[^/]+/[^/]+/pulls$`)
	refForksPath         = regexp.MustCompile(`^/repos/[^/]+/[^/]+/forks$`)
	refItemCommentsPath  = regexp.MustCompile(`^/repos/[^/]+/[^/]+/(?:issues|pulls)/(\d+)/comments$`)
	refGitReceivePackRe  = regexp.MustCompile(`\.git/git-receive-pack$`)
	refGitInfoRefsRe     = regexp.MustCompile(`\.git/info/refs$`)
	referenceLookupLimit = int64(1 << 20)
)

// ReferenceRepoDecision classifies one request against a reference-repo
// policy without touching the network. number is the item for an
// referenceOwnItem decision. reason is the agent-facing refusal for
// referenceDeny.
func ReferenceRepoDecision(pol ReferenceRepoPolicy, method, path, rawQuery string) (d referenceDecision, number int, reason string) {
	push := refGitReceivePackRe.MatchString(path) || (refGitInfoRefsRe.MatchString(path) && gitServiceIsReceivePack(rawQuery))
	if !push && (!writeMethods[method] || gitUploadPackPath.MatchString(path)) {
		return referenceNotReference, 0, ""
	}
	if pol.readOnly() {
		return referenceDeny, 0, referenceRepoRefusal(pol, "it is listed as READ-ONLY")
	}
	if pol.Fork != "" {
		// The owner's fork of a prs-tier entry (hivecommons/hive#18198): the
		// one place an agent may push, since the reference repo itself is not
		// the owner's. Nothing else is done on the fork — the issue, the PR
		// and the comments all belong on the reference repo.
		switch {
		case !pol.PRs:
			return referenceDeny, 0, referenceRepoRefusal(pol, "pushing to the owner's fork needs write: [prs]")
		case push:
			return referencePush, 0, ""
		}
		return referenceDeny, 0, referenceRepoRefusal(pol, "on the owner's fork only pushing a topic branch is allowed — open the PR against "+pol.Repo+" with --head "+pol.Login+":<branch>")
	}
	switch {
	case push:
		if !pol.PRs {
			return referenceDeny, 0, referenceRepoRefusal(pol, "pushing needs write: [prs]")
		}
		return referencePush, 0, ""
	case method == "POST" && refForksPath.MatchString(path):
		if !pol.PRs {
			return referenceDeny, 0, referenceRepoRefusal(pol, "forking needs write: [prs]")
		}
		return referenceAllow, 0, ""
	case method == "POST" && refIssuesPath.MatchString(path):
		if !pol.Issues {
			return referenceDeny, 0, referenceRepoRefusal(pol, "opening an issue needs write: [issues]")
		}
		return referenceAllow, 0, ""
	case method == "POST" && refPullsPath.MatchString(path):
		if !pol.PRs {
			return referenceDeny, 0, referenceRepoRefusal(pol, "opening a PR needs write: [prs]")
		}
		return referenceAllow, 0, ""
	case method == "POST" && refItemCommentsPath.MatchString(path):
		m := refItemCommentsPath.FindStringSubmatch(path)
		n, _ := strconv.Atoi(m[1])
		return referenceOwnItem, n, ""
	case method == "POST" && reviewReplyPathRe.MatchString(path),
		method == "DELETE" && ownCommentDeletePathRe.MatchString(path):
		// A reply inside a review thread, or retracting the hive's own recent
		// comment (#19096). The ownership checks (the hive's own PR and its
		// opening agent, or the hive's own comment inside the retract window)
		// are the review-thread gate's, which runs right after this one.
		return referenceAllow, 0, ""
	}
	return referenceDeny, 0, referenceRepoRefusal(pol, "only opening an issue or PR, forking it, pushing to the owner's fork, commenting on the hive's own items, replying in review threads on the hive's own PRs and retracting the hive's own recent comment are ever allowed there — never merge, push to the default branch, label, assign, close or review")
}

func referenceRepoRefusal(pol ReferenceRepoPolicy, why string) string {
	subject := "repository " + pol.Repo + " is a REFERENCE repo (project.reference_repos, tier " + pol.tier() + ")"
	if pol.Fork != "" {
		subject = "repository " + pol.Fork + " is the hive owner's fork of REFERENCE repo " + pol.Repo + " (project.reference_repos, tier " + pol.tier() + ")"
	}
	return subject + ": " + why + ". Reads — clone, fetch, GET — still work. Put findings about it into the deliverable in your AUTHORIZED repo instead. Do NOT retry, do NOT route around this with another CLI, the hive relays or the GitHub MCP, and do NOT file an issue about it: this is how the operator scoped the hive, not an outage."
}

// SetReferenceRepos installs the reference-repo policies (#18015). Replaces
// the previous set; nil clears it.
func (p *GitHubProxy) SetReferenceRepos(policies []ReferenceRepoPolicy) {
	m := make(map[string]ReferenceRepoPolicy, len(policies))
	for _, pol := range policies {
		key := strings.ToLower(strings.TrimSpace(pol.Repo))
		if key == "" {
			continue
		}
		pol.Repo = strings.TrimSpace(pol.Repo)
		m[key] = pol
	}
	p.referenceMu.Lock()
	p.referenceRepos = m
	p.referenceMu.Unlock()
}

// ReferenceRepoPolicyFor returns the installed policy for repo ("owner/name",
// case-insensitive) and whether one exists. Boot tests read it to show that
// a derived entry whose grant ended no longer has a policy
// (hivecommons/hive#18159); the request path uses referenceRepoFor.
func (p *GitHubProxy) ReferenceRepoPolicyFor(repo string) (ReferenceRepoPolicy, bool) {
	p.referenceMu.RLock()
	defer p.referenceMu.RUnlock()
	pol, ok := p.referenceRepos[strings.ToLower(strings.TrimSpace(repo))]
	return pol, ok
}

// referenceRepoFor returns the policy covering the repo a path names: the
// reference repo itself, or — with Fork set on the returned copy — the hive
// owner's fork of a prs-tier reference repo (hivecommons/hive#18198). A slug
// that is an authorized repo or a reference repo in its own right is never
// read as a fork.
func (p *GitHubProxy) referenceRepoFor(path string) (ReferenceRepoPolicy, bool) {
	p.referenceMu.RLock()
	defer p.referenceMu.RUnlock()
	if len(p.referenceRepos) == 0 {
		return ReferenceRepoPolicy{}, false
	}
	repo := ExtractRepo(path)
	if repo == "" {
		repo = WriteSurfaceRepo(path)
	}
	if repo == "" {
		return ReferenceRepoPolicy{}, false
	}
	key := strings.ToLower(repo)
	if pol, ok := p.referenceRepos[key]; ok {
		return pol, true
	}
	if p.allowedRepos[key] {
		return ReferenceRepoPolicy{}, false
	}
	for _, pol := range p.referenceRepos {
		if pol.PRs && strings.EqualFold(pol.forkSlug(), repo) {
			pol.Fork = repo
			return pol, true
		}
	}
	return ReferenceRepoPolicy{}, false
}

// referenceRepoRefusal is the per-request gate the MITM loop calls. It returns
// the refusal reason and true when the request must be blocked. The own-item
// and default-branch checks consult GitHub through p.referenceLookup; a lookup
// that fails refuses (fail closed) and says so.
func (p *GitHubProxy) referenceRepoRefusal(req *http.Request) (string, bool) {
	pol, ok := p.referenceRepoFor(repoScopePath(req))
	if !ok {
		return "", false
	}
	d, number, reason := ReferenceRepoDecision(pol, req.Method, req.URL.Path, req.URL.RawQuery)
	switch d {
	case referenceNotReference, referenceAllow:
		return "", false
	case referenceDeny:
		return reason, true
	case referenceOwnItem:
		author, err := p.referenceItemAuthor(req, pol, number)
		if err != nil {
			return referenceRepoRefusal(pol, fmt.Sprintf("could not confirm #%d is the hive's own item (%v)", number, err)), true
		}
		if pol.Login == "" || !strings.EqualFold(author, pol.Login) {
			return referenceRepoRefusal(pol, fmt.Sprintf("#%d was opened by %s, not by the hive (%s); comments are allowed only on the hive's own issues and PRs", number, author, pol.Login)), true
		}
		return "", false
	case referencePush:
		if refGitInfoRefsRe.MatchString(req.URL.Path) {
			return "", false // ref advertisement; the refs travel in the POST
		}
		def, err := p.referenceDefaultBranch(req, pol)
		if err != nil {
			return referenceRepoRefusal(pol, fmt.Sprintf("could not resolve its default branch to protect it (%v)", err)), true
		}
		refs, err := peekReceivePackRefs(req)
		if err != nil {
			return referenceRepoRefusal(pol, "push command list unreadable ("+err.Error()+")"), true
		}
		for _, ref := range refs {
			if ref == "refs/heads/"+def {
				return referenceRepoRefusal(pol, "pushing to its default branch ("+def+") is never allowed — push a topic branch and open a PR"), true
			}
		}
		return "", false
	}
	return "", false
}

// referenceWriteCredential says whether a request to a reference repo should
// carry the policy's write token (a write, or the push ref advertisement) —
// every other request to a reference repo goes out unauthenticated.
func referenceWriteCredential(pol ReferenceRepoPolicy, method, path, rawQuery string) bool {
	if pol.Token == nil {
		return false
	}
	d, _, _ := ReferenceRepoDecision(pol, method, path, rawQuery)
	return d != referenceNotReference && d != referenceDeny
}

// referenceAPIBase derives the REST base for the forge host a request is bound
// for: api.github.com for github.com, <host>/api/v3 for GHE.
func referenceAPIBase(req *http.Request) string {
	host := canonicalHost(req.Host)
	if host == "" {
		host = canonicalHost(req.URL.Host)
	}
	switch host {
	case "", "github.com", "api.github.com":
		return "https://api.github.com"
	}
	return "https://" + host + "/api/v3"
}

// referenceLookupFunc is the GET the own-item and default-branch checks use.
// Tests swap it; production uses an ordinary client from this process.
type referenceLookupFunc func(url, token string) ([]byte, error)

func defaultReferenceLookup(url, token string) ([]byte, error) {
	client := &http.Client{Timeout: 10 * time.Second}
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	if token != "" {
		req.Header.Set("Authorization", "token "+token)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, referenceLookupLimit))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("GitHub answered %d", resp.StatusCode)
	}
	return body, nil
}

// referenceLookupOrDefault returns the lookup wrapped in the policy-seam
// guard (#19224): a lookup that panics or outlives referenceLookupTimeout
// answers an error, which every caller already turns into a refusal.
func (p *GitHubProxy) referenceLookupOrDefault() referenceLookupFunc {
	lookup := p.referenceLookup
	if lookup == nil {
		lookup = defaultReferenceLookup
	}
	return func(url, token string) ([]byte, error) {
		type answer struct {
			body []byte
			err  error
		}
		a, ok := callPolicySeam(p, "reference lookup", referenceLookupTimeout, func() answer {
			body, err := lookup(url, token)
			return answer{body, err}
		})
		if !ok {
			return nil, errReferenceLookupFailed
		}
		return a.body, a.err
	}
}

// errReferenceLookupFailed is the error a guarded lookup answers when the
// lookup itself panicked or timed out.
var errReferenceLookupFailed = errors.New("the GitHub lookup failed or timed out")

func (p *GitHubProxy) referenceToken(pol ReferenceRepoPolicy) string {
	if pol.Token == nil {
		return ""
	}
	tok, ok := pol.Token()
	if !ok {
		return ""
	}
	return tok
}

func (p *GitHubProxy) referenceItemAuthor(req *http.Request, pol ReferenceRepoPolicy, number int) (string, error) {
	body, err := p.referenceLookupOrDefault()(referenceAPIBase(req)+"/repos/"+pol.Repo+"/issues/"+strconv.Itoa(number), p.referenceToken(pol))
	if err != nil {
		return "", err
	}
	var item struct {
		User struct {
			Login string `json:"login"`
		} `json:"user"`
	}
	if err := json.Unmarshal(body, &item); err != nil {
		return "", err
	}
	if item.User.Login == "" {
		return "", fmt.Errorf("item has no author")
	}
	return item.User.Login, nil
}

// referenceDefaultBranchTTL bounds how long a resolved default branch is
// trusted before it is looked up again.
const referenceDefaultBranchTTL = 10 * time.Minute

func (p *GitHubProxy) referenceDefaultBranch(req *http.Request, pol ReferenceRepoPolicy) (string, error) {
	key := strings.ToLower(pol.target())
	p.referenceMu.RLock()
	cached, ok := p.referenceDefaults[key]
	p.referenceMu.RUnlock()
	if ok && time.Now().Before(cached.until) {
		return cached.branch, nil
	}
	body, err := p.referenceLookupOrDefault()(referenceAPIBase(req)+"/repos/"+pol.target(), p.referenceToken(pol))
	if err != nil {
		return "", err
	}
	var meta struct {
		DefaultBranch string `json:"default_branch"`
	}
	if err := json.Unmarshal(body, &meta); err != nil {
		return "", err
	}
	if meta.DefaultBranch == "" {
		return "", fmt.Errorf("repository metadata carries no default_branch")
	}
	p.referenceMu.Lock()
	if p.referenceDefaults == nil {
		p.referenceDefaults = map[string]referenceDefaultBranch{}
	}
	p.referenceDefaults[key] = referenceDefaultBranch{branch: meta.DefaultBranch, until: time.Now().Add(referenceDefaultBranchTTL)}
	p.referenceMu.Unlock()
	return meta.DefaultBranch, nil
}

type referenceDefaultBranch struct {
	branch string
	until  time.Time
}

// receivePackPeekLimit bounds the command section read from a push body. The
// command list precedes the packfile and is a handful of lines; anything
// larger is not a push shape this gate will reason about.
const receivePackPeekLimit = 64 * 1024

// peekReceivePackRefs reads the pkt-line command section of a git-receive-pack
// body — "<old> <new> <ref>[\0caps]" lines up to the flush — returns the refs
// it names, and puts every byte back so the request forwards unchanged. A
// content-encoded body cannot be read here and is an error (fail closed).
func peekReceivePackRefs(req *http.Request) ([]string, error) {
	if req.Body == nil {
		return nil, fmt.Errorf("no body")
	}
	if enc := req.Header.Get("Content-Encoding"); enc != "" && enc != "identity" {
		return nil, fmt.Errorf("content-encoding %q", enc)
	}
	br := bufio.NewReader(io.LimitReader(req.Body, receivePackPeekLimit))
	var consumed bytes.Buffer
	tee := io.TeeReader(br, &consumed)
	var refs []string
	for {
		var hdr [4]byte
		if _, err := io.ReadFull(tee, hdr[:]); err != nil {
			return nil, fmt.Errorf("pkt-line header: %w", err)
		}
		n, err := hex.DecodeString(string(hdr[:]))
		if err != nil {
			return nil, fmt.Errorf("pkt-line length %q", hdr)
		}
		size := int(n[0])<<8 | int(n[1])
		if size == 0 {
			break // flush: end of commands
		}
		if size < 4 {
			return nil, fmt.Errorf("pkt-line length %d", size)
		}
		line := make([]byte, size-4)
		if _, err := io.ReadFull(tee, line); err != nil {
			return nil, fmt.Errorf("pkt-line body: %w", err)
		}
		if i := bytes.IndexByte(line, 0); i >= 0 {
			line = line[:i]
		}
		fields := strings.Fields(string(line))
		if len(fields) != 3 {
			return nil, fmt.Errorf("malformed command %q", line)
		}
		refs = append(refs, fields[2])
	}
	// Whatever bufio read ahead past the flush belongs to the packfile; it is
	// also in neither consumed nor req.Body any more, so splice it back too.
	rest, _ := br.Peek(br.Buffered())
	req.Body = io.NopCloser(io.MultiReader(bytes.NewReader(consumed.Bytes()), bytes.NewReader(append([]byte(nil), rest...)), req.Body))
	return refs, nil
}

// referenceWriteAllowedByMode applies the agent's ACMM mode to an opt-in
// reference write: the per-repo opt-in widens WHERE an agent may write, never
// WHAT a mode lets it do. Opening an issue or commenting needs ISSUES_ONLY;
// forking, pushing a branch or opening a PR needs ISSUES_AND_PRS, exactly as on a
// managed repo (the PR hard-deny that routes managed-repo PRs through
// hive-open-pr does not apply — the relay cannot act on a foreign repo, and
// the reference credential is the hive owner's own account).
func referenceWriteAllowedByMode(mode agent.AgentMode, method, path string) bool {
	switch {
	case refGitReceivePackRe.MatchString(path), refGitInfoRefsRe.MatchString(path), refPullsPath.MatchString(path), refForksPath.MatchString(path):
		return mode >= agent.ModeIssuesAndPRs
	}
	return mode >= agent.ModeIssuesOnly
}

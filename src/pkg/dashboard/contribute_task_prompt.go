package dashboard

import (
	"fmt"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	ghpkg "github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/worksource"
)

// buildTaskPrompt constructs the exact assignment prompt sent to a contributor's
// agent for a given issue. It is a PURE function of public task metadata and a
// pre-resolved access mode, and deliberately contains NO credential: the scoped
// github_token is attached to the task_assign WSMessage separately. The text is
// therefore safe to preview read-only in the ops tab (#2539). selectTask stores
// exactly what it ships, so "what is previewed" always matches "what runs".
// buildTaskPrompt is the GitHub-shaped entry point retained for existing call
// sites and tests. Source-aware callers use buildTaskPromptForRef; live dispatch
// uses buildTaskPromptForContributor after resolving the contributor's access.
//
// Two values come from outside the task itself: the base branch (#5729), which
// taskBaseBranch derives from the issue title and this hive's build branch, and
// the access mode (#6654), which selectTask resolves from the connected
// contributor's GitHub identity. Neither is a credential, and the rendered
// result stored on the connection is still the exact prompt sent to the agent.
func buildTaskPrompt(repoFull string, number int, title string) string {
	return buildTaskPromptForRef(worksource.Ref{Repo: repoFull, Number: number}, title)
}

// buildTaskPromptForRef renders the assignment prompt for any work source
// (kubestellar/hive#4245).
//
// The issue reference used to be formatted "%s#%d" straight from repo+number,
// so a Linear or Jira task told the agent to "Work on issue owner/repo#0" — a
// reference to nothing, with the item's real key and URL nowhere in the prompt.
// The reference is now the canonical key, and non-GitHub work additionally
// carries its URL, because that is the only way an agent can actually open it:
// there is no `gh issue view` for a Linear ticket.
//
// Repository instructions always name the GitHub repo; external work is planned
// elsewhere but still landed as a PR here.
func buildTaskPromptForRef(ref worksource.Ref, title string) string {
	return buildTaskPromptForContributor(ref, title, false, "")
}

// buildTaskPromptForContributor renders the checkout workflow selected for the
// contributor's actual access to the target repository. canPush is resolved by
// contributorCanPush immediately before dispatch; the credential remains
// separate from this pure, preview-safe prompt builder.
//
// guide is the ASSIGNING hive's project.writing_guide, already rendered by
// config.ProjectConfig.WritingGuideSection() — the owner's instruction for how
// the PR body should read (hivecommons/hive#8124, extending #7667). It arrives
// as a parameter rather than being read from config here so the builder stays
// pure and preview-safe; the two dispatch call sites pass
// s.deps.Config.Project.WritingGuideSection(). It is the hive's setting and not
// the contributor's, which is the right shape: a relay subscribed to two hives
// gets each hive's guide on that hive's tasks, because the repo owner is who
// decides how PRs in their repo read. Empty renders nothing.
func buildTaskPromptForContributor(ref worksource.Ref, title string, canPush bool, guide string) string {
	repoFull := ref.Repo
	issueRef := ref.Key()
	if issueRef == "" {
		issueRef = repoFull
	}
	// An external item's URL is the agent's only route to the work item itself.
	// GitHub-backed work needs no such hint: `owner/repo#42` is directly
	// actionable, and appending a URL there would change every existing prompt.
	sourceHint := ""
	if !ref.IsGitHubIssue() && ref.URL != "" {
		sourceHint = fmt.Sprintf(
			" This work item lives in the %s work source, not in GitHub Issues; read it at %s.",
			sourceLabel(ref.SourceType), ref.URL)
	}
	// #5729: the prompt has to CARRY the base branch. The checkout is reused
	// across tasks and nothing resets it, so the branch on disk answers the
	// previous task, not this one — and an agent follows the instruction it was
	// given over the state it finds. That was measured, not assumed: a working
	// branch reset from v5 onto v4 mid-task, with zero commits and a clean tree,
	// was put back on v5 by the agent, because the plan it had already formed
	// said v5. Fixing the workspace alone cannot work; the instruction has to
	// carry the answer.
	return buildTaskPromptBodyForAccess(repoFull, issueRef, title, sourceHint,
		taskBaseBranch(title, repoFull, upstreamBranch()), canPush, guide) +
		artifactTrailerPromptInstruction(issueRef)
}

func artifactTrailerPromptInstruction(issueRef string) string {
	issueRef = strings.TrimSpace(issueRef)
	if issueRef == "" {
		return ""
	}
	return " For every commit you create for this implementation stage, include these informational trailers in the commit message: 'Hive-Run: " + issueRef + "', 'Hive-Plan: <plan section or plan name>', and 'Hive-Spec: <spec name>#<clause id>'. Use the plan and Spektacular spec/clause identifiers supplied by the task when available; if one is not available, still include the trailer with the best known identifier."
}

// writingGuideSection renders this hub's own project.writing_guide for the
// assignment prompt, or "" when the hive has not set one
// (hivecommons/hive#8124). Nil-safe at every hop for the same reason
// roleKickPrompt is: the hub is constructed in tests without a full dependency
// graph, and a missing config means "no guide", never a panic on dispatch.
func (h *ContributeWSHub) writingGuideSection() string {
	if h == nil || h.server == nil || h.server.deps == nil || h.server.deps.Config == nil {
		return ""
	}
	return h.server.deps.Config.Project.WritingGuideSection()
}

// taskIDSegment is the per-item component of a task id. For GitHub-backed work
// it is the decimal issue number, which keeps the id exactly the shape it has
// always had; for a string-keyed source it is the native external id, so two
// zero-numbered items minted in the same second get different ids.
func taskIDSegment(ref worksource.Ref) string {
	if ref.Number > 0 {
		return strconv.Itoa(ref.Number)
	}
	return ref.ExternalID
}

// sourceLabel renders a work-source type for prose. An empty type means the
// item came from GitHub enumeration, which predates the field.
func sourceLabel(sourceType string) string {
	if sourceType == "" {
		return "github"
	}
	return sourceType
}

// releaseLineFromTitle extracts a leading release-line tag — "[v5] reviewer
// lane follow-ups …" yields "v5" — and returns "" for every other title.
//
// The shape is deliberately narrow: `v` followed by digits and nothing else,
// the same `^v(\d+)$` release-line shape pkg/hub's image_pulls.go matches and
// .github/release-lines.yml's `release_lines` list uses.
//
// hivecommons/hive#6969: the tag does not have to be the very first token, only
// the first PIECE OF REAL CONTENT. Titles this repo actually files carry the
// tag behind a structured prefix — a classifier lane ("[Tracker] [v5] …") or a
// template emoji ("🔌 [v5] …", "🐛 [Tracker] [v5] …") — and the tag is still an
// unambiguous routing directive there. So the scan skips a bounded run of
// leading bracketed lane tokens and leading decorative (emoji/symbol) runes,
// returning the first bracketed token that is a release line and STOPPING at
// the first ordinary word. A `[v5]` loose in prose ("reviewer lane [v5] …")
// is still rejected, because prose begins with a letter, which ends the scan
// before the tag — that false positive is the one the narrow rule exists to
// prevent and it stays prevented. A tag naming no real branch fails loudly at
// `gh pr create` rather than silently redirecting the PR.
func releaseLineFromTitle(title string) string {
	rest := strings.TrimSpace(title)
	// A title is at most a handful of structured prefixes before real content;
	// the cap keeps the scan bounded regardless of input and never trips on a
	// real title.
	const maxPrefixTokens = 8
	for i := 0; rest != "" && i < maxPrefixTokens; i++ {
		r, size := utf8.DecodeRuneInString(rest)
		switch {
		case r == '[':
			end := strings.Index(rest, "]")
			if end < 0 {
				// An unterminated bracket is not a structured prefix; stop
				// rather than scan into the rest of the title.
				return ""
			}
			tag := strings.ToLower(strings.TrimSpace(rest[1:end]))
			if isReleaseLine(tag) {
				return tag
			}
			// A non-release bracket (a classifier lane like "[quality]"): skip
			// it and keep scanning the following tokens.
			rest = strings.TrimSpace(rest[end+1:])
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			// The first ordinary word or number ends the prefix run. A tag
			// after this point is prose, not a directive.
			return ""
		default:
			// A decorative leading rune — an emoji, variation selector, ZWJ,
			// skin-tone modifier, or punctuation the templates prepend. Skip
			// exactly one rune (multi-rune emoji are skipped a rune at a time)
			// and keep scanning.
			rest = strings.TrimSpace(rest[size:])
		}
	}
	return ""
}

// isReleaseLine reports whether tag is a release-line token: a lowercase `v`
// followed by one or more digits and nothing else (`^v\d+$`).
func isReleaseLine(tag string) bool {
	if len(tag) < 2 || tag[0] != 'v' {
		return false
	}
	for _, r := range tag[1:] {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// hiveOwnRepoName is the repository this hive's own source lives in, and
// hiveOwnRepoOwners are the orgs it has lived under. Both owners are accepted
// because the repo moved orgs: a hive configured before the transfer still
// names "kubestellar", and metrics_collector.go already accepts both for the
// same reason.
//
// Deliberately NOT the same thing as the "hivecommons/hive" literals in api.go.
// Those pin the CANONICAL UPSTREAM the self-version check compares a build
// against, and a fork's binary still checks itself against upstream. This asks
// a different question: is the repository this task is FOR the one whose
// branches this hive's build branch is a statement about.
const hiveOwnRepoName = "hive"

var hiveOwnRepoOwners = []string{"hivecommons", "kubestellar"}

// isHiveOwnRepo reports whether repoFull ("owner/repo") names the repository
// this hive's own source lives in.
//
// An unqualified or malformed value reports false, routing the task to the
// resolve-the-default-branch wording. That is the safe direction in every case:
// that wording is correct for any repository, including this one, where the
// only cost is naming the branch less explicitly than #5729 would like.
// Inheriting a branch on a guess is the failure this exists to prevent.
func isHiveOwnRepo(repoFull string) bool {
	owner, name, found := strings.Cut(strings.TrimSpace(repoFull), "/")
	if !found || !strings.EqualFold(strings.TrimSpace(name), hiveOwnRepoName) {
		return false
	}
	owner = strings.TrimSpace(owner)
	for _, o := range hiveOwnRepoOwners {
		if strings.EqualFold(owner, o) {
			return true
		}
	}
	return false
}

// taskBaseBranch is the branch a task's work must be based on and its PR opened
// against (kubestellar/hive#5729).
//
// A contributor relay works one issue at a time out of a single PERSISTENT
// checkout, and nothing resets that checkout between tasks. The prompt never
// named a base, so the base was whatever the PREVIOUS task happened to leave
// checked out: on 2026-09-02 one `[v5]`-titled issue put the checkout on `v5`
// and the four PRs after it — three of them fixes for defects live on the
// deployed `v4` — were opened against `v5` too, and had to be backported by
// hand. Nobody in the loop can see that going wrong: the agent has nothing to
// check against, the contributor sees PRs opening and merging normally, and a
// maintainer sees correctly-formed PRs on a plausible branch.
//
// hubBranch is the branch this hive itself is built from (upstreamBranch()) —
// the same branch the contribute onboarding page already tells contributors to
// clone (#3990), so "base your work on it" is the answer that was always
// implied and never stated. A branch-specific issue overrides it: an issue
// titled "[v5] …" is work for `v5` whatever branch this hive runs, which is
// also why the inheritance had a plausible-looking first PR to start from.
//
// repoFull is the task's repository. hubBranch is inherited only when that
// repository IS this hive's own source repo (hivecommons/hive#6081). A hive
// dispatching for someone else's repository has no reason to think its own
// build branch means anything there, and the observed result was `v4` stamped
// onto 100% of a self-hosted hive's tasks across four repositories that have no
// such branch. That case self-corrected only because a MISSING branch is loud;
// a repository that happens to have a `v4` which is not its default would have
// resolved, opened the PR against it, and satisfied the prompt's own "confirm
// the base" step. Wrong base, no error, anywhere.
//
// Declining leaves the base empty, which is not a gap: buildTaskPromptBody's
// unresolved-base wording already tells the agent to read the repository's own
// defaultBranchRef, and it keeps the "do not use the branch you find" clause —
// the load-bearing half of #5729 — so both constraints hold at once. That
// wording was previously unreachable, because upstreamBranch() can never return
// empty.
func taskBaseBranch(title, repoFull, hubBranch string) string {
	if line := releaseLineFromTitle(title); line != "" {
		return line
	}
	if !isHiveOwnRepo(repoFull) {
		return ""
	}
	return strings.TrimSpace(hubBranch)
}

// buildTaskPromptBody renders the assignment prompt's text. baseBranch is the
// branch this task's work belongs on; it is empty only when the hive cannot
// resolve one at all, which changes the wording below but never licenses
// inheriting whatever branch the checkout happens to be on.
func buildTaskPromptBody(repoFull, issueRef, title, sourceHint, baseBranch string) string {
	return buildTaskPromptBodyForAccess(repoFull, issueRef, title, sourceHint, baseBranch, false, "")
}

func buildTaskPromptBodyForAccess(repoFull, issueRef, title, sourceHint, baseBranch string, canPush bool, guide string) string {
	// The workspace contract (kubestellar/hive#2545): your tmux pane already
	// starts rooted in $HIVE_WORKSPACE_DIR (contributor-agent.sh creates it and
	// launches the session with -c pointed there), but nothing had put a repo
	// on disk there yet. The previous prompt's only repository instruction was
	// 'gh repo fork ... --clone=false' — a fork WITHOUT a checkout — so an
	// agent that followed it literally, or one that stalled before improvising
	// its own clone, was left sitting in an empty directory while the
	// assignment slot stayed held. Spell out an actual clone into that known
	// directory so there is a concrete first step rather than an implied one.
	baseHint := fmt.Sprintf(
		// An unresolved base is not a licence to inherit one. Keep the "do not
		// use the branch you find" clause, which is the load-bearing half in
		// both wordings, but ASK THE REPOSITORY rather than asserting its
		// answer (hivecommons/hive#7159).
		//
		// This wording used to say "resolve <repo>'s own default branch … and
		// confirm the PR's base is that branch before you report done" — an
		// imperative with a self-check attached, stating a fact about a repo
		// hive cannot see. It is wrong for any repository on a promotion model,
		// where the default branch is the RELEASED line and PRs land on an
		// integration branch. projectbluefin/bluefin says so in its AGENTS.md
		// ("All pull requests target `testing`. Never open a content PR against
		// `main`") and hive agents opened #1275 and #1276 against main anyway,
		// failing its base-branch gate: the agents obeyed the imperative with
		// the self-check over the document with neither. #4928 and #6081 were
		// the same assumption at earlier stages — each fix replaced one wrong
		// assertion with a better one. The remaining gap was asserting at all.
		//
		// The verification step survives, pointed at the repository's
		// requirement instead of at hive's guess: an agent never asked for the
		// base back cannot notice it inherited the wrong one (#5729).
		"Do not assume the branch the checkout is currently on is the right base: it may "+
			"be left over from a previous task. Use the base %s itself requires — check its "+
			"AGENTS.md, CONTRIBUTING and pull-request template for a stated target branch, "+
			"since a repository on a promotion model takes PRs on an integration branch "+
			"rather than on its released default — and only when nothing there names one, "+
			"fall back to its default branch ('gh repo view %s --json defaultBranchRef'). "+
			"Run 'git fetch upstream' and start your work branch from that base with "+
			"'git checkout -b <your-branch> upstream/<base-branch>'. Open the PR against "+
			"the same branch, and confirm before you report done that its base is the branch "+
			"the repository asks for. ",
		repoFull, repoFull)
	if b := strings.TrimSpace(baseBranch); b != "" {
		baseHint = fmt.Sprintf(
			"Base this work on the '%s' branch of %s. The checkout may be left on a "+
				"DIFFERENT branch by a previous task, so do not use whatever branch you "+
				"find there: run 'git fetch upstream' and start your work branch from the "+
				"base with 'git checkout -b <your-branch> upstream/%s'. Open the PR against "+
				"the same branch with 'gh pr create --base %s', and confirm the PR's base "+
				"is '%s' before you report done. ",
			b, repoFull, b, b, b)
	}
	repoOwner, _, _ := strings.Cut(repoFull, "/")
	if repoOwner == "" {
		repoOwner = repoFull
	}
	checkoutHint := fmt.Sprintf(
		"You do NOT have push access to the upstream repo. "+
			"Create the checkout parent with 'mkdir -p $HIVE_WORKSPACE_DIR/%s', "+
			"then get a real checkout on disk: "+
			"'gh repo fork %s --clone=true -- $HIVE_WORKSPACE_DIR/%s' "+
			"(this creates 'origin' for your fork and 'upstream' for the source repo; "+
			"if that directory already has a clone from a prior task, 'cd' into it "+
			"and 'git fetch upstream' instead of re-forking). ",
		repoOwner, repoFull, repoFull)
	pushHint := "Push your branch to your fork's 'origin' remote, then open a PR from your fork. "
	if canPush {
		checkoutHint = fmt.Sprintf(
			"You have push access to the upstream repo, so do not fork it. "+
				"Create the checkout parent with 'mkdir -p $HIVE_WORKSPACE_DIR/%s', "+
				"then get a real checkout on disk: "+
				"'gh repo clone %s $HIVE_WORKSPACE_DIR/%s -- --origin upstream' "+
				"(or, if that directory already has a clone from a prior task, 'cd' "+
				"into it and ensure the 'upstream' remote points to %s before running "+
				"'git fetch upstream'). ",
			repoOwner, repoFull, repoFull, repoFull)
		pushHint = "Push your branch to the 'upstream' remote, then open a PR from that branch. "
	}
	// #7790: the reused checkout's WORKING TREE, not just its branch. The
	// reuse-the-clone clause above says "fetch"; the base-branch clause below
	// says "do not use the branch you find". Neither said anything about
	// uncommitted changes, and a task that is revoked or aborted mid-edit
	// leaves exactly those behind — the relay interrupts the agent and nothing
	// touches the tree. Observed on projectbluefin/utah: one revoked task left
	// three modified files on its branch, and the next four tasks on that repo
	// all started from them. `git checkout -b` carries a dirty tree onto the
	// new branch silently, so one `git add -A` ships another task's half-
	// finished change under this contributor's name. The three PRs that
	// followed leaked nothing only because that agent chose `git worktree add`
	// on its own initiative each time.
	//
	// Stash rather than reset, so an operator can still recover the work; and
	// prefer a worktree, so the shared checkout is never this task's working
	// tree at all. The relay stashes on its own task-exit paths too
	// (preserveTaskLeftovers in bin/contributor-relay.js), but this sentence is
	// what covers a contributor whose relay never sees the checkout.
	checkoutHint += "A checkout left by a prior task may also hold that task's UNCOMMITTED " +
		"changes — a task revoked or aborted mid-edit leaves its half-done edits in " +
		"the tree, and 'git checkout -b' would carry them onto your branch. Before " +
		"creating your branch, run 'git status'; if the tree has changes you did not " +
		"make, set them aside with 'git stash push -u -m \"hive leftover\"' — never " +
		"discard them and never commit them — and confirm the tree is clean. Prefer " +
		"'git worktree add' for your task branch so the shared checkout is never your " +
		"working tree. "

	// hivecommons/hive#8124: project.writing_guide reaches every issue and PR a
	// RESIDENT agent files, through ${WRITING_GUIDE} in the default policy
	// templates (#7667/#7670), but this prompt is built in Go and carried none
	// of it — so an owner who set a guide got it on the quality lane's PRs and
	// not on the PRs a contributor opened for the same repository.
	//
	// Position is the whole point of the setting, and it is the same position
	// here as there: immediately before the step that writes the thing it
	// governs. In a policy template that is the `--body` the agent is told to
	// fill in; on this path it is the open-the-PR instruction below. A style
	// rule that arrives as background loses to the instruction sitting next to
	// the task (#7667), which is why it is not appended as a footer or folded
	// in with the AGENTS.md precedence paragraph above.
	//
	// Set off on its own lines because the guide is free text the owner wrote —
	// possibly several paragraphs — and the rest of this prompt is one running
	// sequence of sentences. Empty renders nothing, so a hive that never set a
	// guide gets a byte-identical prompt, exactly as on the template path.
	guideHint := ""
	if g := strings.TrimSpace(guide); g != "" {
		guideHint = "\n\n" + g + "\n\n"
	}

	return fmt.Sprintf(
		"You are a contributor to the %s hive. Work on issue %s: \"%s\".%s "+
			"%sThen 'cd' into that checkout, read the issue, "+
			"understand what's needed, and take action. "+
			// #7159: precedence. Everything else in this prompt is hive's
			// generic default for a repository hive cannot see; the repo states
			// its own rules in AGENTS.md. Four bot PRs died in one night on
			// projectbluefin repos — two on a base-branch gate, two on
			// conventional-commit title gates — because nothing told the agent
			// which side wins, and hive's wording was the more forceful of the
			// two every time. On this path the repo's AGENTS.md is not even
			// injected (only the scheduler path calls primeAgentsMd), so the
			// prompt has to send the agent to read it.
			"Read the repository's own AGENTS.md and CONTRIBUTING before you start, and "+
			"follow them wherever they conflict with these instructions — PR title format "+
			"(many repositories enforce Conventional Commits and reject a title carrying a "+
			"bracketed prefix), commit message conventions, test and lint commands, review "+
			"etiquette. Two things they do not override: hive's safety rules (never merge "+
			"your own PR, never bypass a write gate), and a branch or requirement this "+
			"assignment names explicitly below. "+
			// #5729: the base branch. Everything above deliberately REUSES a
			// checkout across tasks, which is exactly what makes the branch
			// left on disk the previous task's answer rather than this one's.
			// Name the branch, name it before the agent forms a plan, and ask
			// for the base back at the end — an agent never told a base cannot
			// notice it inherited the wrong one.
			"%s"+
			// DCO is enforced on this repo (CONTRIBUTING.md) and an unsigned
			// commit blocks the merge, but the prompt used to leave sign-off
			// entirely to whatever each agent inferred from the repo. That
			// varies even within one backend: two agy tasks produced one
			// unsigned PR (#4127, fixed by hand) and one signed (#4176). Every
			// other required step here is spelled out; this one was the
			// exception, so say it.
			"Commit with 'git commit -s' so every commit carries a Signed-off-by "+
			"trailer — the DCO check blocks the merge without it, and the trailer's "+
			"email must match the commit author's email. "+
			// #8124: the assigning hive's writing guide, immediately before the
			// instruction to open the PR whose body it governs. Empty renders
			// nothing.
			"%s"+
			// Open it READY, not draft. A draft trips the repo's
			// do-not-merge/work-in-progress automation and tide will not merge
			// one, so a draft left behind is a PR nobody is waiting on and
			// nothing will land. The prompt used to say only "open a PR", which
			// an agent can satisfy perfectly well with `gh pr create --draft`.
			//
			// Observed live: the task for #4188 scoped down to child #4205 and
			// ran `gh pr create --draft`, then verified `isDraft: true` and
			// reported "Draft PR #4242 is open" as a SUCCESSFUL handoff — it had
			// no reason to think otherwise. The contributor had to intervene by
			// hand ("don't make them draft. Make sure they are submitted and
			// ready for review") to get it marked ready.
			"%s"+
			"Open it ready for review, not as a draft (do not pass --draft): a draft "+
			"is auto-labelled do-not-merge/work-in-progress and cannot merge. If a PR "+
			"is already open as a draft, mark it ready for review. "+
			"Use the GH_TOKEN env var for all gh commands (do NOT use 'unset GITHUB_TOKEN'). "+
			// #3987: the no_work_needed sentinel. The relay scrapes this exact
			// marker from the agent's output and reports it as the completion
			// verdict, so an issue whose remainder is maintainer-gated stops
			// re-entering the offer pool every cooldown window. Keep the marker
			// spelling in sync with detectNoWorkVerdict in
			// bin/contributor-relay.js.
			"If you determine there is genuinely NOTHING shippable — for example the "+
			"merged PRs already cover everything actionable — do NOT open a PR; instead print "+
			"a single line of plain text, no Markdown formatting, in the exact form "+
			"'HIVE_VERDICT: no_work_needed — <short reason>' "+
			"(for already-done issues, include verifiable evidence such as "+
			"'merged PR #123 already resolves this' or 'already fixed by commit <sha>' "+
			"only after checking the PR is merged or the commit is on the default branch) "+
			"and stop. If the remaining work is waiting on a maintainer-only design, "+
			"policy, or approval decision, use the structured reason form "+
			"'HIVE_VERDICT: no_work_needed — decision: <what needs deciding>' "+
			"so hive applies the configured maintainer-decision label and keeps it out "+
			"of the queue until a human removes that label. "+
			// hivecommons/hive#7924: the blocked sentinel. utah#100 reached a
			// correct "nothing here can change until utah-packages' factory
			// publishes" and printed no_work_needed for it; the hub booked an
			// ordinary no-PR completion and re-ran the same research on the 4h
			// backoff. A verdict that SAYS blocked is held for the full cooldown
			// and, when the credential allows, labelled `blocked` by the relay
			// (markIssueBlocked in bin/contributor-relay.js) so the existing
			// admission gate takes over. Keep the spelling in sync with the relay.
			"If instead nothing in this repository can change until something OUTSIDE "+
			"it lands — another repository's release or build, a dependency that has "+
			"not published yet, an external service; NOT a maintainer decision — print "+
			"'HIVE_VERDICT: blocked — <what it is waiting on>' "+
			"instead of no_work_needed and stop: hive then holds the issue for the "+
			"full cooldown and applies the repository's 'blocked' label, which a human "+
			"lifts when the dependency clears. "+
			// #7924 (second half): leave the finding on GitHub, not just in this
			// hub's ledger. fsdk-containers#299 was correctly no_work_needed
			// (fix in open PR #289) with nothing linking the two on GitHub —
			// no text ref, no sidebar link — so the merge could not close it and
			// the next cycle had to re-verify. The comment creates the
			// cross-reference and tells a human why; the relay does the label.
			"Before printing either of those two lines, if your GH_TOKEN can write to "+
			"issues (it can for issue tasks; a 403 means it cannot, in which case skip "+
			"this), leave ONE comment on the issue naming exactly what covers or blocks "+
			"it — the open or merged PR, the commit, or the external dependency — so "+
			"the finding survives on GitHub and the PR is cross-referenced to the "+
			"issue; sign it the same way as a PR body. Never edit someone else's PR "+
			"body to add 'Fixes #N': if that PR's merge should close this issue, say so "+
			"in a comment on the PR instead, noting that only a maintainer editing the "+
			"body makes the merge close it. "+
			// #5376: the completion sentinel. The interactive relay used to
			// infer "this task is done" from the CLI's own terminal chrome —
			// per-backend regexes over the last fifteen lines of the tmux pane.
			// That produced thirteen separate issues (#1566, #4026, #4064,
			// #4067, #4078, #4080, #4128, #4182, #4265, #5094, #5121, #5156,
			// #5162) as one CLI after another restyled its output, because the
			// input was a vendor's cosmetic rendering rather than a contract.
			// This line IS the contract: the agent states it is finished. The
			// relay's detectCompletionVerdict scrapes it; keep the marker
			// spelling in sync with bin/contributor-relay.js.
			//
			// Asked for LAST and on its own line for a reason: the relay reads
			// a bounded tail of the pane, so a sentinel buried above a long
			// summary can scroll out of view before the relay looks.
			"When you HAVE finished the task — the PR is open, or you have "+
			"otherwise done everything you intend to do — print, as the very "+
			"last thing you output and on a line by itself, in plain text, no Markdown formatting: "+
			"'HIVE_VERDICT: complete — <short reason>'. Print it exactly once, "+
			"only when you are actually done, and never before starting work. "+
			// #7841: a background shell the agent left alive re-enters it when
			// the shell exits (Claude Code delivers the output as a task
			// notification and the model takes another turn), so a verdict
			// printed with shells still running is followed by more work after
			// the relay has credited the task. Stated as a rule so the CLI's own
			// "N shells still running" chrome on the verdict line reads as a
			// violation rather than a race.
			"Before printing it, stop or wait for every background shell or "+
			"job you started — nothing you launched may still be running when "+
			"the verdict line appears, because its completion would wake you "+
			"for another turn after the task has been credited. "+
			// #7858: a careful agent's instinct — or a contributor's standing
			// instructions — is to verify before declaring done, so it parks on
			// CI gates and review bots for minutes with a PR URL on the pane and
			// no verdict. The relay cannot tell that from a stall, and hive's
			// periodic PR review cycle already owns post-PR follow-up, so the
			// wait is pure cost. Say so explicitly.
			"Opening the PR IS finishing: do not wait for CI, checks, or review "+
			"bots to report before printing the verdict — hive reviews and "+
			"follows up on open PRs separately, so any wait here only holds the "+
			"task. "+
			"If you printed the no_work_needed, decision, or blocked line above, that already "+
			"counts as your completion — do not print both. "+
			// #7759: the one sanctioned second verdict. A CLI that runs a
			// passive reviewer (omp's --advisor) posts its notes on the agent's
			// FINAL turn under the sentinel, after the agent has stopped, so
			// the relay asks it once to address them and re-print the line.
			// Without this sentence that request contradicts "exactly once"
			// and a careful agent may refuse it. The bound is the relay's
			// (maybeRequestPostVerdictReview in bin/contributor-relay.js):
			// one follow-up per task, and the second verdict is final.
			"One exception: if your CLI runs a reviewer or advisor whose notes "+
			"appear after your verdict, you may be asked once to address the "+
			"ones that apply and print the HIVE_VERDICT line again — do so; that "+
			"second line is expected, and it is final.",
		repoFull, issueRef, title, sourceHint, checkoutHint, baseHint, guideHint, pushHint,
	)
}

// attributionPromptInstruction renders the footer instruction appended to an
// assignment prompt (kubestellar/hive#4105). It tells the agent — up front, using
// the hub's handshake-recorded invocation values — the exact attribution trailer
// its PR body must end with, so agent-produced PRs carry the footer without
// waiting for the hub-side EnsurePRAttribution reconciliation (#4088), which
// stays in place unchanged as an idempotent safety net. Like the rest of the
// prompt this is a pure function of non-credential metadata, so it remains safe
// to preview in the ops tab (#2539). Returns "" when the metadata renders no
// trailer at all (all fields unknown) — a bare prefix instruction would ask the
// agent to produce an empty footer.
func attributionPromptInstruction(meta ghpkg.InvocationMeta) string {
	trailer := meta.Trailer()
	if trailer == "" {
		return ""
	}
	// #7924: the same trailer signs an issue comment the agent leaves at a
	// no_work_needed/blocked verdict, so a reader can tell which hive said it.
	return " At the bottom of your PR body — and of any issue comment you leave — include exactly this line: '" + trailer + "'."
}

// promptInvocationMeta snapshots the hub's own handshake-recorded invocation
// metadata for a connection (never relay-self-reported values), mirroring the
// meta built by reconcilePRAttribution so the prompt instruction (#4105) and the
// post-merge reconciliation trailer (#4088) always agree. Caller must NOT hold
// c.mu. Nil-safe: a nil connection yields zero metadata (and hence no footer
// instruction).
func promptInvocationMeta(c *ContributorConnection) ghpkg.InvocationMeta {
	if c == nil {
		return ghpkg.InvocationMeta{}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	meta := ghpkg.InvocationMeta{
		Agent:   c.role,
		Backend: c.cliBackend,
		Model:   ghpkg.RequestedModel(c.cliBackend, c.model),
		Effort:  c.reasoningEffort,
	}
	return meta
}

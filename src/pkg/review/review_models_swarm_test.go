package review

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
)

func poolReviewer(name string, pool ...config.ReviewModelPoolEntry) AgentCapability {
	r := reviewer(name)
	r.Backend, r.Model = "copilot", "claude-fable-5.1"
	r.ReviewModels = config.ReviewModelsConfig{Pool: pool, ExcludeAuthorFamily: true}
	return r
}

func swarmOpts(agents ...AgentCapability) DispatchOptions {
	return DispatchOptions{RequireApproval: true, FanOut: true, ProjectOrg: "acme", MaxParallelReviews: 5, Agents: agents}
}

func TestSwarmKickSelectsCrossFamilyModelFromPool(t *testing.T) {
	pr := dispatchPR("sha1")
	pr.AuthorModel = "claude-fable-5.1"
	agent := poolReviewer("r1",
		config.ReviewModelPoolEntry{Backend: "copilot", Model: "claude-opus-4-6"},
		config.ReviewModelPoolEntry{Backend: "copilot", Model: "gpt-6-astra"},
		config.ReviewModelPoolEntry{Backend: "copilot", Model: "gemini-4-argon"},
	)
	plan := PlanDispatch([]PullRequest{pr}, Artifact{}, DispatchState{}, swarmOpts(agent))
	if len(plan.ReviewKicks) != 1 {
		t.Fatalf("kicks=%d want 1", len(plan.ReviewKicks))
	}
	k := plan.ReviewKicks[0]
	if k.ReviewBackend != "copilot" || k.ReviewModel != "gpt-6-astra" || k.ReviewModelFallback {
		t.Fatalf("kick model = %s/%s fallback=%v; want copilot/gpt-6-astra from pool", k.ReviewBackend, k.ReviewModel, k.ReviewModelFallback)
	}
	if k.AuthorModel != "claude-fable-5.1" {
		t.Fatalf("kick author model = %q", k.AuthorModel)
	}
	for _, want := range []string{
		IndependentReviewHeading,
		"Run this review with `copilot/gpt-6-astra`",
		"`model` set to `gpt-6-astra`",
		"Author model: claude-fable-5.1",
		`Set author_model to "claude-fable-5.1" and review_model to "gpt-6-astra"`,
	} {
		if !strings.Contains(k.Message, want) {
			t.Fatalf("perspective prompt missing %q:\n%s", want, k.Message)
		}
	}
	if p := plan.State.Pending[0]; p.AuthorModel != "claude-fable-5.1" || p.ReviewModel != "gpt-6-astra" {
		t.Fatalf("pending stamp = %+v", p)
	}
}

func TestSwarmKickPinnedFallbackWhenPoolExhausted(t *testing.T) {
	pr := dispatchPR("sha1")
	pr.AuthorModel = "gpt-6-astra"
	agent := poolReviewer("r1", config.ReviewModelPoolEntry{Backend: "copilot", Model: "gpt-5.6-sol"})
	plan := PlanDispatch([]PullRequest{pr}, Artifact{}, DispatchState{}, swarmOpts(agent))
	if len(plan.ReviewKicks) != 1 {
		t.Fatalf("kicks=%d want 1", len(plan.ReviewKicks))
	}
	k := plan.ReviewKicks[0]
	if k.ReviewModel != "claude-fable-5.1" || !k.ReviewModelFallback {
		t.Fatalf("pinned fallback not applied: %+v", k)
	}
	if !strings.Contains(k.Message, "Run this review with `copilot/claude-fable-5.1`") {
		t.Fatalf("prompt does not name the pinned model:\n%s", k.Message)
	}
}

func TestSwarmKickSkipFallbackWithholdsPerspective(t *testing.T) {
	pr := dispatchPR("sha1")
	pr.AuthorModel = "gpt-6-astra"
	agent := poolReviewer("r1", config.ReviewModelPoolEntry{Backend: "copilot", Model: "gpt-5.6-sol"})
	agent.ReviewModels.Fallback = config.ReviewModelsFallbackSkip
	plan := PlanDispatch([]PullRequest{pr}, Artifact{}, DispatchState{}, swarmOpts(agent))
	if len(plan.ReviewKicks) != 0 || len(plan.State.Pending) != 0 {
		t.Fatalf("skip fallback still dispatched: kicks=%d pending=%d", len(plan.ReviewKicks), len(plan.State.Pending))
	}
	if len(plan.SkippedReviewModels) != 1 || plan.SkippedReviewModels[0].Agent != "r1" || plan.SkippedReviewModels[0].AuthorModel != "gpt-6-astra" {
		t.Fatalf("skip not recorded: %+v", plan.SkippedReviewModels)
	}
}

func TestSwarmKickRequiresHumanFallbackHoldsPR(t *testing.T) {
	pr := dispatchPR("sha1")
	pr.AuthorModel = "gpt-6-astra"
	agent := poolReviewer("r1", config.ReviewModelPoolEntry{Backend: "copilot", Model: "gpt-5.6-sol"})
	agent.ReviewModels.Fallback = config.ReviewModelsFallbackRequiresHuman
	plan := PlanDispatch([]PullRequest{pr}, Artifact{}, DispatchState{}, swarmOpts(agent))
	if len(plan.ReviewKicks) != 0 {
		t.Fatalf("requires_human fallback still dispatched %d kicks", len(plan.ReviewKicks))
	}
	if len(plan.State.Human) != 1 || !strings.Contains(plan.State.Human[0].Reason, "no independent review model") || !strings.Contains(plan.State.Human[0].Reason, "gpt-6-astra") {
		t.Fatalf("human hold not recorded: %+v", plan.State.Human)
	}
}

func TestSwarmKickWithoutPoolCarriesNoModelInstruction(t *testing.T) {
	pr := dispatchPR("sha1")
	pr.AuthorModel = "claude-fable-5.1"
	agent := reviewer("r1")
	agent.Backend, agent.Model = "copilot", "claude-fable-5.1"
	plan := PlanDispatch([]PullRequest{pr}, Artifact{}, DispatchState{}, swarmOpts(agent))
	if len(plan.ReviewKicks) != 1 {
		t.Fatalf("kicks=%d want 1", len(plan.ReviewKicks))
	}
	k := plan.ReviewKicks[0]
	if k.ReviewModel != "" || strings.Contains(k.Message, IndependentReviewHeading) {
		t.Fatalf("no pool must mean no model instruction: model=%q\n%s", k.ReviewModel, k.Message)
	}
	// author_model is still stamped: the pair is recorded even on a hive
	// with no pool, which is what makes a later pool measurable.
	if !strings.Contains(k.Message, "Author model: claude-fable-5.1") || !strings.Contains(k.Message, `Set author_model to "claude-fable-5.1".`) {
		t.Fatalf("author model not stamped without a pool:\n%s", k.Message)
	}
	if p := plan.State.Pending[0]; p.AuthorModel != "claude-fable-5.1" || p.ReviewModel != "claude-fable-5.1" {
		t.Fatalf("pending stamp should fall back to the agent's launch model: %+v", p)
	}
}

func TestSwarmPerspectiveModelsOverridePool(t *testing.T) {
	pr := dispatchPR("sha1")
	pr.AuthorModel = "claude-fable-5.1"
	agent := poolReviewer("r1", config.ReviewModelPoolEntry{Backend: "copilot", Model: "gpt-6-astra"})
	opts := swarmOpts(agent)
	opts.PerspectiveModels = map[string]config.ReviewModelPoolEntry{
		string(PerspectiveCorrectness): {Backend: "copilot", Model: "gemini-4-argon"},
	}
	plan := PlanDispatch([]PullRequest{pr}, Artifact{}, DispatchState{}, opts)
	if len(plan.ReviewKicks) != 1 || plan.ReviewKicks[0].Perspective != PerspectiveCorrectness {
		t.Fatalf("expected one correctness kick, got %+v", plan.ReviewKicks)
	}
	if got := plan.ReviewKicks[0].ReviewModel; got != "gemini-4-argon" {
		t.Fatalf("perspective override ignored: review model %q", got)
	}

	// The override is still subject to the agent's exclusions: a pinned
	// model from the author's family falls back like an exhausted pool.
	opts.PerspectiveModels[string(PerspectiveCorrectness)] = config.ReviewModelPoolEntry{Backend: "copilot", Model: "claude-opus-4-6"}
	plan = PlanDispatch([]PullRequest{pr}, Artifact{}, DispatchState{}, opts)
	if k := plan.ReviewKicks[0]; k.ReviewModel != "claude-fable-5.1" || !k.ReviewModelFallback {
		t.Fatalf("override escaped exclude_author_family: %+v", k)
	}

	// An override applies even when the agent has no review_models block,
	// under the default exclusions and pinned fallback.
	plain := reviewer("r2")
	plain.Backend, plain.Model = "copilot", "claude-fable-5.1"
	opts = swarmOpts(plain)
	opts.PerspectiveModels = map[string]config.ReviewModelPoolEntry{string(PerspectiveCorrectness): {Backend: "copilot", Model: "gpt-6-astra"}}
	plan = PlanDispatch([]PullRequest{pr}, Artifact{}, DispatchState{}, opts)
	if k := plan.ReviewKicks[0]; k.ReviewModel != "gpt-6-astra" {
		t.Fatalf("override without a pool ignored: %+v", k)
	}
}

func TestCombinedKickHonoursPool(t *testing.T) {
	pr := dispatchPR("sha1")
	pr.AuthorModel = "claude-fable-5.1"
	agent := poolReviewer("r1", config.ReviewModelPoolEntry{Backend: "copilot", Model: "gpt-6-astra"})
	opts := swarmOpts(agent)
	opts.CombinedPerspectives = true
	plan := PlanDispatch([]PullRequest{pr}, Artifact{}, DispatchState{}, opts)
	if len(plan.ReviewKicks) != 1 {
		t.Fatalf("kicks=%d want 1", len(plan.ReviewKicks))
	}
	k := plan.ReviewKicks[0]
	if k.ReviewModel != "gpt-6-astra" || !strings.Contains(k.Message, "Run this review with `copilot/gpt-6-astra`") {
		t.Fatalf("combined kick did not carry the pool model: %+v", k)
	}
	for _, p := range plan.State.Pending {
		if p.ReviewModel != "gpt-6-astra" || p.AuthorModel != "claude-fable-5.1" {
			t.Fatalf("pending stamp = %+v", p)
		}
	}
}

func TestIndependentReviewSectionsShareOneBody(t *testing.T) {
	queue := QueueIndependentReviewSection()
	swarm := PerspectiveIndependentReviewSection("copilot", "gpt-6-astra")
	for _, shared := range []string{
		IndependentReviewHeading,
		"launch a sub-agent (Agent tool) with `model` set to",
		"invented APIs, tests that assert the implementation rather than the requirement, and swallowed errors",
		"Record the sub-agent's verdict and model in the verdict artifact as `review_model`",
		"Never review a PR yourself with the author's model.",
	} {
		if !strings.Contains(queue, shared) || !strings.Contains(swarm, shared) {
			t.Fatalf("shared clause %q missing from one path\nqueue: %s\nswarm: %s", shared, queue, swarm)
		}
	}
	if !strings.Contains(queue, "review_with=<backend>/<model>") {
		t.Fatalf("queue section lost its per-line clause: %s", queue)
	}
	if PerspectiveIndependentReviewSection("copilot", "") != "" {
		t.Fatal("no model must yield no section")
	}
	if got := PerspectiveIndependentReviewSection("", "gpt-6-astra"); !strings.Contains(got, "Run this review with `gpt-6-astra`") {
		t.Fatalf("backendless target: %s", got)
	}
}

func TestModelStampsFillOmittedReportFields(t *testing.T) {
	state := DispatchState{
		Pending: []PendingReview{{Repo: "acme/hive", Number: 7, HeadSHA: "sha1", Perspective: PerspectiveSecurity, Agent: "r1", AuthorModel: "claude-fable-5.1", ReviewModel: "gpt-6-astra"}},
		Recent:  []RecentReview{{Repo: "acme/hive", Number: 7, HeadSHA: "sha1", Perspective: PerspectiveCorrectness, Agent: "r1", AuthorModel: "claude-fable-5.1", ReviewModel: "gemini-4-argon"}},
	}
	stamps := ModelStampsFromState(state)
	if len(stamps) != 1 {
		t.Fatalf("stamps=%d want 1 head", len(stamps))
	}

	omitted := &PerspectiveReport{Repo: "acme/hive", Number: 7, HeadSHA: "sha1", BodyDigest: "d1", Perspective: PerspectiveSecurity}
	stamps.apply(omitted)
	if omitted.AuthorModel != "claude-fable-5.1" || omitted.ReviewModel != "gpt-6-astra" {
		t.Fatalf("security report not stamped: %+v", omitted)
	}
	other := &PerspectiveReport{Repo: "acme/hive", Number: 7, HeadSHA: "sha1", Perspective: PerspectiveCorrectness}
	stamps.apply(other)
	if other.ReviewModel != "gemini-4-argon" {
		t.Fatalf("recent review stamp not applied per perspective: %+v", other)
	}

	written := &PerspectiveReport{Repo: "acme/hive", Number: 7, HeadSHA: "sha1", Perspective: PerspectiveSecurity, AuthorModel: "gpt-5.6-sol", ReviewModel: "grok-4.7"}
	stamps.apply(written)
	if written.AuthorModel != "gpt-5.6-sol" || written.ReviewModel != "grok-4.7" {
		t.Fatalf("reviewer-written models were overwritten: %+v", written)
	}

	unknown := &PerspectiveReport{Repo: "acme/hive", Number: 8, HeadSHA: "sha9", Perspective: PerspectiveSecurity}
	stamps.apply(unknown)
	if unknown.AuthorModel != "" || unknown.ReviewModel != "" {
		t.Fatalf("unrelated head was stamped: %+v", unknown)
	}
	if ModelStampsFromState(DispatchState{}) != nil {
		t.Fatal("empty state must yield nil stamps")
	}
}

func TestCollectStampsVerdictArtifact(t *testing.T) {
	dir := t.TempDir()
	report := `{"lane":"review-swarm","kind":"review","findings":[],"prs_opened":[],"beads_filed":[],"summary":"ok","perspective":"security","verdict":"approve","repo":"acme/hive","number":7,"head_sha":"sha1"}`
	if err := os.WriteFile(filepath.Join(dir, ReviewReportFilePrefix+"security"+ReviewReportFileSuffix), []byte(report), 0o644); err != nil {
		t.Fatal(err)
	}
	stamps := ModelStampsFromState(DispatchState{Pending: []PendingReview{{Repo: "acme/hive", Number: 7, HeadSHA: "sha1", Perspective: PerspectiveSecurity, Agent: "r1", AuthorModel: "claude-fable-5.1", ReviewModel: "gpt-6-astra", Dispatched: time.Now()}}})
	art, err := Collect(dir, AggregateOptions{ModelStamps: stamps})
	if err != nil {
		t.Fatal(err)
	}
	if len(art.Items) != 1 || art.Items[0].AuthorModel != "claude-fable-5.1" || art.Items[0].ReviewModel != "gpt-6-astra" {
		t.Fatalf("artifact not stamped: %+v", art.Items)
	}
	plain, err := Collect(dir, AggregateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if plain.Items[0].AuthorModel != "" || plain.Items[0].ReviewModel != "" {
		t.Fatalf("nil stamps must leave the report as written: %+v", plain.Items[0])
	}
}

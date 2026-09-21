package commands

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/hivectl"
)

type fakeRegistrar struct {
	calls  []string
	result hubRegistration
	err    error
}

func (f *fakeRegistrar) Register(_ context.Context, base, user string) (hubRegistration, error) {
	f.calls = append(f.calls, base+" "+user)
	return f.result, f.err
}

type hivesHarness struct {
	dir          string
	store        *hivectl.ProfileStore
	reg          *fakeRegistrar
	out          *bytes.Buffer
	errs         *bytes.Buffer
	in           *bytes.Reader
	signalResult hivectl.RelaySwitchResult
	signalErr    error
	signalCalls  int
}

// newHivesHarness points `hivectl hives` at a temporary config directory and a
// fake hub, so no test ever touches the developer's real credentials or the
// network.
func newHivesHarness(t *testing.T) *hivesHarness {
	t.Helper()
	dir := t.TempDir()
	h := &hivesHarness{
		dir:   dir,
		store: hivectl.NewProfileStore(dir),
		reg:   &fakeRegistrar{result: hubRegistration{RegistrationToken: "new-token", ContributorID: "contrib_new", Message: "registered"}},
		out:   &bytes.Buffer{},
		errs:  &bytes.Buffer{},
		in:    bytes.NewReader(nil),
	}
	prev := hivesDepsFor
	hivesDepsFor = func(*commandEnv) (*hivesDeps, error) {
		return &hivesDeps{
			store:      h.store,
			registrar:  h.reg,
			githubUser: func(context.Context) (string, error) { return "octocat", nil },
			signalRelay: func(context.Context, *hivectl.ProfileStore) (hivectl.RelaySwitchResult, error) {
				h.signalCalls++
				return h.signalResult, h.signalErr
			},
			now: func() time.Time { return time.Date(2026, 9, 21, 10, 0, 0, 0, time.UTC) },
		}, nil
	}
	t.Cleanup(func() { hivesDepsFor = prev })
	return h
}

func (h *hivesHarness) run(t *testing.T, stdin string, args ...string) error {
	t.Helper()
	h.out.Reset()
	h.errs.Reset()
	root := NewRootCommand(strings.NewReader(stdin), h.out, h.errs)
	root.SetArgs(args)
	return root.Execute()
}

func (h *hivesHarness) writeEnv(t *testing.T, contents string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(h.dir, "contributor.env"), []byte(contents), 0o600); err != nil {
		t.Fatalf("write contributor.env: %v", err)
	}
}

func (h *hivesHarness) seed(t *testing.T, set *hivectl.ProfileSet) {
	t.Helper()
	if err := h.store.Save(set); err != nil {
		t.Fatalf("seed profiles: %v", err)
	}
}

func (h *hivesHarness) profiles(t *testing.T) *hivectl.ProfileSet {
	t.Helper()
	set, err := h.store.Load()
	if err != nil {
		t.Fatalf("load profiles: %v", err)
	}
	if set == nil {
		t.Fatal("no profiles.yml was written")
	}
	return set
}

func (h *hivesHarness) env(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(h.dir, "contributor.env"))
	if err != nil {
		t.Fatalf("read contributor.env: %v", err)
	}
	return string(data)
}

func twoHives() *hivectl.ProfileSet {
	return &hivectl.ProfileSet{
		Active: "acme",
		Profiles: []hivectl.Profile{
			{Name: "acme", Hub: "wss://acme.example/contribute", ContributorID: "c1", RegistrationToken: "tok-acme"},
			{Name: "other", Hub: "wss://other.example/contribute", ContributorID: "c2", RegistrationToken: "tok-other"},
		},
	}
}

func TestHivesListMarksTheActiveHive(t *testing.T) {
	h := newHivesHarness(t)
	h.seed(t, twoHives())

	if err := h.run(t, "", "hives", "list"); err != nil {
		t.Fatalf("hives list: %v", err)
	}
	out := h.out.String()
	for _, want := range []string{"acme", "other", "wss://acme.example/contribute", "c1"} {
		if !strings.Contains(out, want) {
			t.Errorf("hives list output missing %q:\n%s", want, out)
		}
	}
	// The active hive is first and carries the marker; the list order is the
	// order the relay will walk.
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) < 3 || !strings.HasPrefix(lines[1], "*") {
		t.Errorf("active marker not on the first row:\n%s", out)
	}
	if strings.Index(out, "acme") > strings.Index(out, "other") {
		t.Errorf("active hive is not listed first:\n%s", out)
	}
}

func TestHivesListShowsLastSeen(t *testing.T) {
	h := newHivesHarness(t)
	h.seed(t, twoHives())
	seen := `{"wss://acme.example/contribute":"2026-09-21T15:04:05Z"}`
	if err := os.WriteFile(h.store.HubsSeenPath(), []byte(seen), 0o600); err != nil {
		t.Fatalf("write hubs-seen: %v", err)
	}

	if err := h.run(t, "", "hives", "list"); err != nil {
		t.Fatalf("hives list: %v", err)
	}
	out := h.out.String()
	if !strings.Contains(out, "LAST SEEN") || !strings.Contains(out, "2026-09-21T15:04:05Z") {
		t.Fatalf("last-seen column missing:\n%s", out)
	}
}

// A token pasted into a terminal or a CI log is a token that has to be
// reissued. No output format may print one.
func TestHivesListNeverPrintsTokens(t *testing.T) {
	for _, format := range []string{"table", "json", "yaml", "jsonl"} {
		t.Run(format, func(t *testing.T) {
			h := newHivesHarness(t)
			h.seed(t, twoHives())
			if err := h.run(t, "", "hives", "list", "-o", format); err != nil {
				t.Fatalf("hives list -o %s: %v", format, err)
			}
			if strings.Contains(h.out.String(), "tok-acme") || strings.Contains(h.out.String(), "tok-other") {
				t.Fatalf("hives list -o %s printed a registration token:\n%s", format, h.out.String())
			}
		})
	}
}

func TestHivesListJSONShape(t *testing.T) {
	h := newHivesHarness(t)
	h.seed(t, twoHives())
	if err := h.run(t, "", "hives", "list", "-o", "json"); err != nil {
		t.Fatalf("hives list -o json: %v", err)
	}
	var rows []map[string]any
	if err := json.Unmarshal(h.out.Bytes(), &rows); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, h.out.String())
	}
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want 2", len(rows))
	}
	if rows[0]["name"] != "acme" || rows[0]["active"] != true {
		t.Errorf("first row = %+v, want the active hive", rows[0])
	}
	if rows[1]["active"] != false {
		t.Errorf("second row = %+v, want active false", rows[1])
	}
	if _, present := rows[0]["reachable"]; present {
		t.Error("reachable was reported without --check")
	}
}

// Acceptance (#8097): a contributor with the old positional file runs ANY
// hives command, gets a migrated profiles.yml, and the relay's configuration
// is untouched.
func TestHivesListMigratesLegacyEnvAndLeavesTheRelayAlone(t *testing.T) {
	h := newHivesHarness(t)
	legacy := "HIVE_REGISTRATION_TOKEN=t1,t2\nHIVE_HUB=wss://acme.example/contribute,wss://other.example/contribute\nCONTRIBUTOR_ID=c1,c2\nAGENT_BACKEND=claude\n"
	h.writeEnv(t, legacy)

	if err := h.run(t, "", "hives", "list"); err != nil {
		t.Fatalf("hives list: %v", err)
	}
	if !strings.Contains(h.errs.String(), "Migrated 2 hive(s)") {
		t.Errorf("migration was not reported on stderr: %q", h.errs.String())
	}
	set := h.profiles(t)
	if len(set.Profiles) != 2 || set.Active != "acme" {
		t.Fatalf("migrated set = %+v", set)
	}
	if got := h.env(t); got != legacy {
		t.Errorf("migration changed contributor.env:\ngot:\n%s\nwant:\n%s", got, legacy)
	}
}

func TestHivesListRefusesMisalignedLegacyEnv(t *testing.T) {
	h := newHivesHarness(t)
	h.writeEnv(t, "HIVE_HUB=wss://a.example/contribute,wss://b.example/contribute\nHIVE_REGISTRATION_TOKEN=t1\n")
	err := h.run(t, "", "hives", "list")
	if err == nil || !strings.Contains(err.Error(), "misaligned") {
		t.Fatalf("error = %v, want a refusal naming the misalignment", err)
	}
}

func TestHivesListWithNothingConfigured(t *testing.T) {
	h := newHivesHarness(t)
	err := h.run(t, "", "hives", "list")
	if err == nil || !strings.Contains(err.Error(), "no hives configured yet") {
		t.Fatalf("error = %v, want setup guidance", err)
	}
}

func TestHivesUseReordersTheProjection(t *testing.T) {
	h := newHivesHarness(t)
	h.seed(t, twoHives())
	h.writeEnv(t, "HIVE_REGISTRATION_TOKEN=tok-acme,tok-other\nHIVE_HUB=wss://acme.example/contribute,wss://other.example/contribute\nCONTRIBUTOR_ID=c1,c2\nHIVE_LITELLM_ENDPOINT=https://llm.example\n")

	if err := h.run(t, "", "hives", "use", "other"); err != nil {
		t.Fatalf("hives use: %v", err)
	}
	if got := h.profiles(t).Active; got != "other" {
		t.Errorf("active = %q, want other", got)
	}
	env := h.env(t)
	// The relay starts at activeHubIndex 0, so "use" means "project first".
	if !strings.Contains(env, "HIVE_HUB=wss://other.example/contribute,wss://acme.example/contribute") {
		t.Errorf("hub list not reordered:\n%s", env)
	}
	if !strings.Contains(env, "HIVE_REGISTRATION_TOKEN=tok-other,tok-acme") {
		t.Errorf("token list not reordered with the hubs:\n%s", env)
	}
	if !strings.Contains(env, "CONTRIBUTOR_ID=c2,c1") {
		t.Errorf("id list not reordered with the hubs:\n%s", env)
	}
	if !strings.Contains(env, "HIVE_LITELLM_ENDPOINT=https://llm.example") {
		t.Errorf("unmanaged key dropped from the projection:\n%s", env)
	}
	if h.signalCalls != 1 {
		t.Fatalf("signal relay calls = %d, want 1", h.signalCalls)
	}
	if !strings.Contains(h.out.String(), "no running relay found") {
		t.Errorf("output did not say no relay was running:\n%s", h.out.String())
	}
}

func TestHivesUseSignalsRunningRelay(t *testing.T) {
	h := newHivesHarness(t)
	h.signalResult = hivectl.RelaySwitchResult{Running: true, Target: "pid 1234"}
	h.seed(t, twoHives())
	h.writeEnv(t, "HIVE_REGISTRATION_TOKEN=tok-acme,tok-other\nHIVE_HUB=wss://acme.example/contribute,wss://other.example/contribute\nCONTRIBUTOR_ID=c1,c2\n")

	if err := h.run(t, "", "hives", "use", "other"); err != nil {
		t.Fatalf("hives use: %v", err)
	}
	if !strings.Contains(h.out.String(), "running relay signaled (pid 1234)") {
		t.Fatalf("output did not report the live switch:\n%s", h.out.String())
	}
	if !strings.Contains(h.out.String(), "in-flight work finishes on its original hive") {
		t.Fatalf("output did not document in-flight task handling:\n%s", h.out.String())
	}
}

func TestHivesUseUnknownName(t *testing.T) {
	h := newHivesHarness(t)
	h.seed(t, twoHives())
	err := h.run(t, "", "hives", "use", "nope")
	if err == nil || !errors.Is(err, hivectl.ErrProfileNotFound) {
		t.Fatalf("error = %v, want ErrProfileNotFound", err)
	}
	if !strings.Contains(err.Error(), "acme, other") {
		t.Errorf("error did not list the configured hives: %v", err)
	}
	if h.profiles(t).Active != "acme" {
		t.Error("a failed 'use' changed the active hive")
	}
}

func TestHivesAddRegistersAndAppends(t *testing.T) {
	h := newHivesHarness(t)
	h.seed(t, twoHives())

	if err := h.run(t, "", "hives", "add", "third", "--hub", "wss://third.example/contribute"); err != nil {
		t.Fatalf("hives add: %v", err)
	}
	if len(h.reg.calls) != 1 || h.reg.calls[0] != "https://third.example octocat" {
		t.Fatalf("registrar calls = %v, want one POST to the hub's HTTP base as octocat", h.reg.calls)
	}
	set := h.profiles(t)
	if len(set.Profiles) != 3 {
		t.Fatalf("got %d profiles, want 3", len(set.Profiles))
	}
	added, _ := set.Find("third")
	if added == nil || added.RegistrationToken != "new-token" || added.ContributorID != "contrib_new" {
		t.Fatalf("added profile = %+v", added)
	}
	// Adding does not steal the active slot from a hive already in use.
	if set.Active != "acme" {
		t.Errorf("active = %q, want the previously active hive", set.Active)
	}
	// Existing credentials survive the append — the bug #4408 fixed for
	// contribute-setup must not come back through a new path.
	env := h.env(t)
	for _, want := range []string{"tok-acme", "tok-other", "new-token"} {
		if !strings.Contains(env, want) {
			t.Errorf("projection lost %q:\n%s", want, env)
		}
	}
}

// Adding a hive on a machine still carrying the positional file must migrate
// first, or the append would land on an empty set and the existing hives'
// tokens — which the hub cannot reprint — would vanish from the projection.
func TestHivesAddMigratesBeforeAppending(t *testing.T) {
	h := newHivesHarness(t)
	h.writeEnv(t, "HIVE_REGISTRATION_TOKEN=t1\nHIVE_HUB=wss://legacy.example/contribute\nCONTRIBUTOR_ID=c1\nAGENT_BACKEND=claude\n")

	if err := h.run(t, "", "hives", "add", "third", "--hub", "wss://third.example/contribute"); err != nil {
		t.Fatalf("hives add: %v", err)
	}
	if !strings.Contains(h.errs.String(), "Migrated 1 hive(s)") {
		t.Errorf("migration was not reported: %q", h.errs.String())
	}
	set := h.profiles(t)
	if len(set.Profiles) != 2 {
		t.Fatalf("got %d profiles, want the migrated one plus the new one: %+v", len(set.Profiles), set.Profiles)
	}
	if set.Active != "legacy" {
		t.Errorf("active = %q; adding must not move a running relay off its hub", set.Active)
	}
	env := h.env(t)
	if !strings.Contains(env, "HIVE_REGISTRATION_TOKEN=t1,new-token") {
		t.Errorf("projection lost the pre-existing credential:\n%s", env)
	}
	if !strings.Contains(env, "AGENT_BACKEND=claude") {
		t.Errorf("projection dropped an unmanaged key:\n%s", env)
	}
}

func TestHivesAddActivateMakesItActive(t *testing.T) {
	h := newHivesHarness(t)
	h.seed(t, twoHives())
	if err := h.run(t, "", "hives", "add", "third", "--hub", "wss://third.example/contribute", "--activate"); err != nil {
		t.Fatalf("hives add --activate: %v", err)
	}
	set := h.profiles(t)
	if set.Active != "third" {
		t.Fatalf("active = %q, want third", set.Active)
	}
	if !strings.HasPrefix(h.env(t), "HIVE_REGISTRATION_TOKEN=new-token,") {
		t.Errorf("the activated hive was not projected first:\n%s", h.env(t))
	}
}

// The first hive added on a fresh machine has to become active, or the relay
// would have no hub to start from.
func TestHivesAddFirstProfileBecomesActive(t *testing.T) {
	h := newHivesHarness(t)
	if err := h.run(t, "", "hives", "add", "first", "--hub", "wss://first.example/contribute"); err != nil {
		t.Fatalf("hives add: %v", err)
	}
	if got := h.profiles(t).Active; got != "first" {
		t.Errorf("active = %q, want first", got)
	}
}

func TestHivesAddTokenStdinSkipsRegistration(t *testing.T) {
	h := newHivesHarness(t)
	err := h.run(t, "  moved-token\n", "hives", "add", "moved", "--hub", "wss://moved.example/contribute",
		"--token-stdin", "--contributor-id", "contrib_moved")
	if err != nil {
		t.Fatalf("hives add --token-stdin: %v", err)
	}
	if len(h.reg.calls) != 0 {
		t.Errorf("--token-stdin still contacted the hub: %v", h.reg.calls)
	}
	added, _ := h.profiles(t).Find("moved")
	if added == nil || added.RegistrationToken != "moved-token" || added.ContributorID != "contrib_moved" {
		t.Fatalf("added profile = %+v", added)
	}
}

func TestHivesAddValidation(t *testing.T) {
	for _, tt := range []struct {
		name  string
		stdin string
		args  []string
		want  string
	}{
		{name: "hub required", args: []string{"hives", "add", "x"}, want: "--hub is required"},
		{name: "hub scheme", args: []string{"hives", "add", "x", "--hub", "ftp://x.example"}, want: "must use ws://"},
		{name: "hub with a comma", args: []string{"hives", "add", "x", "--hub", "wss://a.example/contribute,wss://b.example/contribute"}, want: "comma"},
		{name: "bad name", args: []string{"hives", "add", "a/b", "--hub", "wss://x.example/contribute"}, want: "may use only letters"},
		{name: "contributor id needs token-stdin", args: []string{"hives", "add", "x", "--hub", "wss://x.example/contribute", "--contributor-id", "c"}, want: "applies only with --token-stdin"},
		{name: "empty stdin token", args: []string{"hives", "add", "x", "--hub", "wss://x.example/contribute", "--token-stdin"}, want: "stdin was empty"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			h := newHivesHarness(t)
			err := h.run(t, tt.stdin, tt.args...)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want one containing %q", err, tt.want)
			}
			if _, statErr := os.Stat(h.store.Path()); !os.IsNotExist(statErr) {
				t.Error("a rejected add still wrote profiles.yml")
			}
		})
	}
}

func TestHivesAddRejectsDuplicateName(t *testing.T) {
	h := newHivesHarness(t)
	h.seed(t, twoHives())
	err := h.run(t, "", "hives", "add", "ACME", "--hub", "wss://dup.example/contribute")
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("error = %v, want a duplicate-name refusal", err)
	}
	if len(h.profiles(t).Profiles) != 2 {
		t.Error("a rejected add still appended a profile")
	}
}

// The hub refuses to reprint an existing contributor's token, and that refusal
// is correct (register is unauthenticated). What the operator needs is the
// supported way forward, not a bare failure.
func TestHivesAddAlreadyRegisteredNamesTheWayForward(t *testing.T) {
	h := newHivesHarness(t)
	h.reg.result = hubRegistration{Message: "already registered"}
	err := h.run(t, "", "hives", "add", "acme", "--hub", "wss://acme.example/contribute")
	if err == nil {
		t.Fatal("hives add succeeded with no token")
	}
	for _, want := range []string{"already registered", "--token-stdin", "contribute-move", "unauthenticated"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error missing %q: %v", want, err)
		}
	}
	if _, statErr := os.Stat(h.store.Path()); !os.IsNotExist(statErr) {
		t.Error("a failed registration still wrote profiles.yml")
	}
}

func TestHivesAddSurfacesRegistrationFailure(t *testing.T) {
	h := newHivesHarness(t)
	h.reg.err = errors.New("hub unreachable")
	err := h.run(t, "", "hives", "add", "x", "--hub", "wss://x.example/contribute")
	if err == nil || !strings.Contains(err.Error(), "hub unreachable") {
		t.Fatalf("error = %v, want the registrar's failure", err)
	}
}

func TestHivesRename(t *testing.T) {
	h := newHivesHarness(t)
	h.seed(t, twoHives())
	if err := h.run(t, "", "hives", "rename", "acme", "acme-prod"); err != nil {
		t.Fatalf("hives rename: %v", err)
	}
	set := h.profiles(t)
	if p, _ := set.Find("acme-prod"); p == nil {
		t.Fatal("renamed profile is missing")
	}
	// Renaming the active hive must carry the active marker with it, or the
	// set would name a profile that no longer exists.
	if set.Active != "acme-prod" {
		t.Errorf("active = %q, want acme-prod", set.Active)
	}
}

func TestHivesRenameRejectsACollision(t *testing.T) {
	h := newHivesHarness(t)
	h.seed(t, twoHives())
	err := h.run(t, "", "hives", "rename", "acme", "other")
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("error = %v, want a collision refusal", err)
	}
	if p, _ := h.profiles(t).Find("acme"); p == nil {
		t.Error("a rejected rename still changed the profile")
	}
}

// "acme" -> "Acme" collides with itself under case-insensitive uniqueness; it
// must still be allowed.
func TestHivesRenameAllowsACaseChange(t *testing.T) {
	h := newHivesHarness(t)
	h.seed(t, twoHives())
	if err := h.run(t, "", "hives", "rename", "acme", "Acme"); err != nil {
		t.Fatalf("hives rename: %v", err)
	}
	set := h.profiles(t)
	if set.Profiles[0].Name != "Acme" || set.Active != "Acme" {
		t.Errorf("set = %+v", set)
	}
}

func TestHivesRemoveRequiresConfirmation(t *testing.T) {
	h := newHivesHarness(t)
	h.seed(t, twoHives())
	if err := h.run(t, "not-the-name\n", "hives", "remove", "acme"); err != nil {
		t.Fatalf("hives remove: %v", err)
	}
	if len(h.profiles(t).Profiles) != 2 {
		t.Fatal("an unconfirmed remove discarded a registration token the hub cannot reprint")
	}
	if !strings.Contains(h.out.String(), "Left \"acme\" in place") {
		t.Errorf("output did not say the hive was kept:\n%s", h.out.String())
	}
}

func TestHivesRemoveConfirmed(t *testing.T) {
	h := newHivesHarness(t)
	h.seed(t, twoHives())
	h.writeEnv(t, "HIVE_REGISTRATION_TOKEN=tok-acme,tok-other\nHIVE_HUB=wss://acme.example/contribute,wss://other.example/contribute\nCONTRIBUTOR_ID=c1,c2\n")

	if err := h.run(t, "acme\n", "hives", "remove", "acme"); err != nil {
		t.Fatalf("hives remove: %v", err)
	}
	set := h.profiles(t)
	if len(set.Profiles) != 1 || set.Profiles[0].Name != "other" {
		t.Fatalf("profiles after remove = %+v", set.Profiles)
	}
	// Removing the active hive hands the active slot to a surviving one, so
	// the relay still has a hub to start from.
	if set.Active != "other" {
		t.Errorf("active = %q, want other", set.Active)
	}
	env := h.env(t)
	if strings.Contains(env, "tok-acme") {
		t.Errorf("removed hive's token is still in the projection:\n%s", env)
	}
	if !strings.Contains(env, "HIVE_HUB=wss://other.example/contribute") {
		t.Errorf("projection = %s", env)
	}
	// The discarded credential is still recoverable from the backup, because
	// the hub will not reprint it.
	bak, err := os.ReadFile(filepath.Join(h.dir, "contributor.env.bak"))
	if err != nil {
		t.Fatalf("read backup: %v", err)
	}
	if !strings.Contains(string(bak), "tok-acme") {
		t.Errorf("backup did not keep the removed credential:\n%s", bak)
	}
}

func TestHivesRemoveYesSkipsThePrompt(t *testing.T) {
	h := newHivesHarness(t)
	h.seed(t, twoHives())
	if err := h.run(t, "", "hives", "remove", "other", "--yes"); err != nil {
		t.Fatalf("hives remove --yes: %v", err)
	}
	if len(h.profiles(t).Profiles) != 1 {
		t.Fatal("--yes did not remove the hive")
	}
}

func TestHivesRemoveLastHiveSaysSo(t *testing.T) {
	h := newHivesHarness(t)
	h.seed(t, &hivectl.ProfileSet{Active: "only", Profiles: []hivectl.Profile{{Name: "only", Hub: "wss://only.example/contribute", RegistrationToken: "t"}}})
	if err := h.run(t, "", "hives", "remove", "only", "--yes"); err != nil {
		t.Fatalf("hives remove: %v", err)
	}
	set := h.profiles(t)
	if len(set.Profiles) != 0 || set.Active != "" {
		t.Fatalf("set = %+v", set)
	}
	if !strings.Contains(h.out.String(), "no hives left") {
		t.Errorf("output did not warn that nothing is left:\n%s", h.out.String())
	}
}

func TestHivesRemoveUnknownName(t *testing.T) {
	h := newHivesHarness(t)
	h.seed(t, twoHives())
	err := h.run(t, "", "hives", "remove", "nope", "--yes")
	if err == nil || !errors.Is(err, hivectl.ErrProfileNotFound) {
		t.Fatalf("error = %v, want ErrProfileNotFound", err)
	}
}

func TestHivesListCheckProbesTheHub(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/contribute/status" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer up.Close()

	h := newHivesHarness(t)
	h.seed(t, &hivectl.ProfileSet{
		Active: "up",
		Profiles: []hivectl.Profile{
			{Name: "up", Hub: strings.Replace(up.URL, "http://", "ws://", 1) + "/contribute", RegistrationToken: "t1"},
			// Port 0 is never listenable, so this one is reliably unreachable
			// without depending on a name that might resolve.
			{Name: "down", Hub: "ws://127.0.0.1:0/contribute", RegistrationToken: "t2"},
		},
	})
	if err := h.run(t, "", "hives", "list", "--check", "-o", "json"); err != nil {
		t.Fatalf("hives list --check: %v", err)
	}
	var rows []map[string]any
	if err := json.Unmarshal(h.out.Bytes(), &rows); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, h.out.String())
	}
	if rows[0]["reachable"] != true {
		t.Errorf("a hub that answered was reported as %v", rows[0]["reachable"])
	}
	if rows[1]["reachable"] != false {
		t.Errorf("a hub that cannot be dialled was reported as %v", rows[1]["reachable"])
	}
}

// The registrar must not forward any bearer credential to a hub URL that can
// come from a registry entry (#4408, H7/CWE-522).
func TestHTTPRegistrarSendsNoCredential(t *testing.T) {
	var gotAuth, gotBody string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		body, _ := io.ReadAll(r.Body)
		gotBody = string(body)
		_, _ = w.Write([]byte(`{"registration_token":"tok","contributor_id":"c","message":"registered"}`))
	}))
	defer server.Close()

	reg, err := httpRegistrar{timeout: 5 * time.Second}.Register(context.Background(), server.URL, "octocat")
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if reg.RegistrationToken != "tok" || reg.ContributorID != "c" {
		t.Errorf("registration = %+v", reg)
	}
	if gotAuth != "" {
		t.Errorf("Register sent an Authorization header (%q); the hub URL is attacker-influenceable", gotAuth)
	}
	if !strings.Contains(gotBody, `"github_username":"octocat"`) {
		t.Errorf("request body = %q", gotBody)
	}
}

func TestHTTPRegistrarReportsHTTPErrors(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("hub is down"))
	}))
	defer server.Close()

	_, err := httpRegistrar{timeout: 5 * time.Second}.Register(context.Background(), server.URL, "octocat")
	if err == nil || !strings.Contains(err.Error(), "503") || !strings.Contains(err.Error(), "hub is down") {
		t.Fatalf("error = %v, want one naming the status and body", err)
	}
}

func TestHTTPRegistrarRejectsNonJSON(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("<html>login</html>"))
	}))
	defer server.Close()

	_, err := httpRegistrar{timeout: 5 * time.Second}.Register(context.Background(), server.URL, "octocat")
	if err == nil || !strings.Contains(err.Error(), "non-JSON") {
		t.Fatalf("error = %v, want a non-JSON complaint", err)
	}
}

// The production wiring resolves the profile store under $HOME/.config/hive
// and the GitHub login through gh. Both are exercised against a throwaway
// HOME and a stubbed gh, so the default path is covered without touching the
// developer's real credentials.
func TestDefaultHivesDepsWiresTheProductionPieces(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	deps, err := defaultHivesDeps(3 * time.Second)
	if err != nil {
		t.Fatalf("defaultHivesDeps: %v", err)
	}
	if deps.store == nil || deps.registrar == nil || deps.githubUser == nil || deps.now == nil {
		t.Fatalf("defaultHivesDeps left a dependency nil: %+v", deps)
	}
	if reg, ok := deps.registrar.(httpRegistrar); !ok || reg.timeout != 3*time.Second {
		t.Errorf("registrar = %#v, want httpRegistrar with the given timeout", deps.registrar)
	}

	// With no override installed, commandEnv.hivesDeps() takes the same path.
	prev := hivesDepsFor
	hivesDepsFor = nil
	t.Cleanup(func() { hivesDepsFor = prev })
	env := &commandEnv{options: &rootOptions{timeout: 3 * time.Second}}
	got, err := env.hivesDeps()
	if err != nil || got == nil {
		t.Fatalf("hivesDeps() = %v, %v", got, err)
	}
}

// The GitHub-login lookup itself moved to pkg/hivectl with the `gh`
// subprocess; it is pinned there (TestGitHubLogin). What this asserts is the
// WIRING — that the production deps resolve the login through that shared
// function rather than through a second copy of the lookup.
func TestDefaultHivesDepsUsesTheSharedGitHubLogin(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	deps, err := defaultHivesDeps(3 * time.Second)
	if err != nil {
		t.Fatalf("defaultHivesDeps: %v", err)
	}
	old := hivectl.RunGH
	t.Cleanup(func() { hivectl.RunGH = old })
	hivectl.RunGH = func(_ context.Context, args ...string) (string, error) {
		if strings.Join(args, " ") != "api user --jq .login" {
			t.Errorf("unexpected gh invocation: %v", args)
		}
		return "octocat\n", nil
	}
	if user, err := deps.githubUser(context.Background()); err != nil || user != "octocat" {
		t.Errorf("deps.githubUser = %q, %v; want octocat", user, err)
	}
}

func TestHivesListTableShowsReachabilityAndDashes(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer up.Close()

	h := newHivesHarness(t)
	h.seed(t, &hivectl.ProfileSet{
		Active: "up",
		Profiles: []hivectl.Profile{
			{Name: "up", Hub: strings.Replace(up.URL, "http://", "ws://", 1) + "/contribute", RegistrationToken: "t1", ContributorID: "contrib_up"},
			{Name: "down", Hub: "ws://127.0.0.1:0/contribute", RegistrationToken: "t2"},
		},
	})
	if err := h.run(t, "", "hives", "list", "--check"); err != nil {
		t.Fatalf("hives list --check: %v", err)
	}
	out := h.out.String()
	for _, want := range []string{"REACHABLE", "* = active", "yes", "no"} {
		if !strings.Contains(out, want) {
			t.Errorf("table output lacks %q:\n%s", want, out)
		}
	}
	// The profile with no contributor id or session prints dashes, not blanks.
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "down") && !strings.Contains(line, "\t-\t-\t") {
			t.Errorf("empty columns should print as dashes: %q", line)
		}
	}
	if strings.Contains(out, "t1") || strings.Contains(out, "t2") {
		t.Errorf("table output leaked a registration token:\n%s", out)
	}
}

func TestUnknownHiveErrorListsWhatIsConfigured(t *testing.T) {
	err := unknownHiveError("nope", &hivectl.ProfileSet{})
	if !errors.Is(err, hivectl.ErrProfileNotFound) || !strings.Contains(err.Error(), "no hives are configured") {
		t.Errorf("empty set: %v", err)
	}
	err = unknownHiveError("nope", &hivectl.ProfileSet{Profiles: []hivectl.Profile{{Name: "a"}, {Name: "b"}}})
	if !strings.Contains(err.Error(), "configured: a, b") {
		t.Errorf("populated set should list names: %v", err)
	}
}

func TestAlreadyRegisteredErrorDefaultsTheMessage(t *testing.T) {
	err := alreadyRegisteredError("octocat", "https://hub", "", "acme", "wss://hub/contribute")
	if !strings.Contains(err.Error(), "the hub returned no registration token") {
		t.Errorf("empty message should get the default wording: %v", err)
	}
	if !strings.Contains(err.Error(), "--token-stdin") || !strings.Contains(err.Error(), "contribute-move") {
		t.Errorf("error should name both ways forward: %v", err)
	}
}

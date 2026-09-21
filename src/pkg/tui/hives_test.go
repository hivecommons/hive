package tui

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/hivecommons/hive/pkg/hivectl"
	"github.com/hivecommons/hive/pkg/tui/client"
	"github.com/hivecommons/hive/pkg/tui/panes"
)

// The Hives overlay's app-side tests (#8128).
//
// They drive the REAL profile store over a temporary config directory rather
// than a fake, because the acceptance criterion is about effect on disk: "enter
// on an entry has the same effect as `hivectl hives use`". A mock store would
// assert that a method was called; this asserts that profiles.yml and the
// contributor.env projection say afterwards exactly what the CLI would have
// made them say.
//
// Nothing here touches $HOME or the network: the store is built over t.TempDir
// and the probe/login/register dependencies are injected.

var hivesKey = tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("H")}

func key(s string) tea.KeyMsg { return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)} }

// hivesHarness is a sized model whose hives dependencies point at a temporary
// config directory.
type hivesHarness struct {
	model    model
	dir      string
	store    *hivectl.ProfileStore
	probes   atomic.Int64
	probeErr map[string]bool // hub -> unreachable
	logins   atomic.Int64
	regs     atomic.Int64
	regFn    func(ctx context.Context, base, user string) (hivectl.Registration, error)
}

// seededHives is the fixture: two hives, "acme" active, matching the shape
// `hivectl hives list` prints in its docs.
func seededHives() *hivectl.ProfileSet {
	return &hivectl.ProfileSet{
		Version: hivectl.ProfilesVersion,
		Active:  "acme",
		Profiles: []hivectl.Profile{
			{Name: "acme", Hub: "wss://acme.example/contribute", ContributorID: "contrib_a1", RegistrationToken: "tok-acme"},
			{Name: "other", Hub: "wss://other.example/contribute", ContributorID: "contrib_b2", RegistrationToken: "tok-other", Session: "review"},
		},
	}
}

func newHivesHarness(t *testing.T, set *hivectl.ProfileSet) *hivesHarness {
	t.Helper()
	dir := t.TempDir()
	h := &hivesHarness{
		dir:      dir,
		store:    hivectl.NewProfileStore(dir),
		probeErr: map[string]bool{},
	}
	if set != nil {
		if err := h.store.Save(set); err != nil {
			t.Fatalf("seed profiles: %v", err)
		}
	}
	m := newModel()
	m.width, m.height = 110, 34
	// A fleet snapshot is delivered so that a key LEAKING out of the overlay
	// would have found an agent to act on — which is what makes the modal
	// containment assertions below mean something.
	next, _ := m.Update(panes.AgentsMsg{Agents: []client.Agent{
		{Name: "scanner", DisplayName: "Scanner", Enabled: true, Backend: "claude", Model: "claude-opus-4-5"},
	}})
	m = next.(model)
	m.hivesEnv = hivesEnv{
		store: h.store,
		probe: func(_ context.Context, hub string) bool {
			h.probes.Add(1)
			return !h.probeErr[hub]
		},
		login: func(context.Context) (string, error) {
			h.logins.Add(1)
			return "octocat", nil
		},
		register: func(ctx context.Context, base, user string) (hivectl.Registration, error) {
			h.regs.Add(1)
			if h.regFn != nil {
				return h.regFn(ctx, base, user)
			}
			return hivectl.Registration{RegistrationToken: "tok-new", ContributorID: "contrib_new"}, nil
		},
	}
	h.model = m
	return h
}

// send delivers a message and keeps the resulting model, returning the command
// it produced so the caller can run it.
func (h *hivesHarness) send(t *testing.T, msg tea.Msg) tea.Cmd {
	t.Helper()
	next, cmd := h.model.Update(msg)
	h.model = next.(model)
	return cmd
}

// run executes a command and feeds every message it produced back into the
// model. tea.Batch returns a BatchMsg carrying the individual Cmds, which is
// how the per-hub probes arrive.
func (h *hivesHarness) run(t *testing.T, cmd tea.Cmd) {
	t.Helper()
	if cmd == nil {
		return
	}
	msg := cmd()
	switch batch := msg.(type) {
	case tea.BatchMsg:
		for _, c := range batch {
			h.run(t, c)
		}
	case nil:
	default:
		h.run(t, h.send(t, msg))
	}
}

// open presses H and settles the profile read and the probes.
func (h *hivesHarness) open(t *testing.T) {
	t.Helper()
	cmd := h.send(t, hivesKey)
	if h.model.hives == nil {
		t.Fatal("H did not open the Hives overlay")
	}
	h.run(t, cmd)
}

func (h *hivesHarness) view() string { return h.model.View() }

func (h *hivesHarness) profiles(t *testing.T) *hivectl.ProfileSet {
	t.Helper()
	set, err := h.store.Load()
	if err != nil {
		t.Fatalf("load profiles: %v", err)
	}
	if set == nil {
		t.Fatal("no profiles.yml on disk")
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

// ── the list ────────────────────────────────────────────────────────────────

// TestHivesPaneRendersTheListWithTheActiveMarker is the first acceptance
// criterion: the pane shows the same rows `hivectl hives list` does, active
// first and marked.
func TestHivesPaneRendersTheListWithTheActiveMarker(t *testing.T) {
	h := newHivesHarness(t, seededHives())
	h.open(t)

	view := h.view()
	for _, want := range []string{"acme", "other", "wss://acme.example/contribute", "contrib_a1", "review"} {
		if !strings.Contains(view, want) {
			t.Errorf("the Hives overlay does not show %q:\n%s", want, view)
		}
	}
	if strings.Index(view, "acme") > strings.Index(view, "other") {
		t.Errorf("the active hive is not listed first:\n%s", view)
	}
	rows := h.model.hives.Rows()
	if len(rows) != 2 || !rows[0].Active || rows[1].Active {
		t.Fatalf("rows = %+v, want acme active and first", rows)
	}
	if !strings.Contains(view, "* = active") {
		t.Errorf("the active marker is not explained:\n%s", view)
	}
}

// A registration token reaching the frame would be a credential pasted into an
// operator's scrollback — and, because the frame is what golden files pin, into
// the repository. The row type does not carry one; this fails if that changes.
func TestHivesPaneNeverRendersARegistrationToken(t *testing.T) {
	h := newHivesHarness(t, seededHives())
	h.open(t)
	view := h.view()
	for _, secret := range []string{"tok-acme", "tok-other"} {
		if strings.Contains(view, secret) {
			t.Fatalf("the Hives overlay rendered a registration token (%s):\n%s", secret, view)
		}
	}
}

// TestHivesPaneMigratesALegacyEnvFile: opening the pane is as good a "first
// hives command" as running the CLI, so a machine still carrying the old
// positional contributor.env is migrated — and the relay's configuration is
// left byte-identical, because migration alone must not rewrite it.
func TestHivesPaneMigratesALegacyEnvFile(t *testing.T) {
	h := newHivesHarness(t, nil)
	legacy := "HIVE_REGISTRATION_TOKEN=t1,t2\n" +
		"HIVE_HUB=wss://acme.example/contribute,wss://other.example/contribute\n" +
		"CONTRIBUTOR_ID=c1,c2\nAGENT_BACKEND=claude\n"
	if err := os.WriteFile(filepath.Join(h.dir, "contributor.env"), []byte(legacy), 0o600); err != nil {
		t.Fatalf("write legacy env: %v", err)
	}

	h.open(t)
	if rows := h.model.hives.Rows(); len(rows) != 2 || rows[0].Name != "acme" {
		t.Fatalf("rows = %+v, want the two migrated hives with acme first", rows)
	}
	if got := h.env(t); got != legacy {
		t.Errorf("opening the pane rewrote contributor.env:\ngot:\n%s\nwant:\n%s", got, legacy)
	}
}

// A machine with nothing configured is not an error state to hide — it is the
// state in which `a` is the only useful key, so the overlay has to say so.
func TestHivesPaneWithNothingConfigured(t *testing.T) {
	h := newHivesHarness(t, nil)
	h.open(t)
	view := h.view()
	if !strings.Contains(view, "no hives configured") {
		t.Errorf("an unconfigured machine is not explained:\n%s", view)
	}
	if !strings.Contains(view, "a add") {
		t.Errorf("the add key is not offered on an empty machine:\n%s", view)
	}
}

// ── enter: the use path ─────────────────────────────────────────────────────

// TestHivesEnterSwitchesTheActiveHive is THE acceptance criterion of #8128:
// enter on a row must have the same effect as `hivectl hives use`. That effect
// is defined on disk — profiles.yml records the new active hive and
// contributor.env is regenerated with it first in all three positional lists —
// so that is what this asserts, not that a function was called.
func TestHivesEnterSwitchesTheActiveHive(t *testing.T) {
	h := newHivesHarness(t, seededHives())
	h.open(t)

	h.run(t, h.send(t, key("j"))) // move to "other"
	row, ok := h.model.hives.Selected()
	if !ok || row.Name != "other" {
		t.Fatalf("selected = %+v (ok=%v), want other", row, ok)
	}
	h.run(t, h.send(t, tea.KeyMsg{Type: tea.KeyEnter}))

	if got := h.profiles(t).Active; got != "other" {
		t.Fatalf("active hive on disk = %q, want other", got)
	}
	env := h.env(t)
	for _, want := range []string{
		"HIVE_HUB=wss://other.example/contribute,wss://acme.example/contribute",
		"HIVE_REGISTRATION_TOKEN=tok-other,tok-acme",
		"CONTRIBUTOR_ID=contrib_b2,contrib_a1",
	} {
		if !strings.Contains(env, want) {
			t.Errorf("contributor.env is missing %q:\n%s", want, env)
		}
	}

	// The overlay reloads, so the marker on screen agrees with the file, and
	// the receipt is honest about the relay needing a restart (phase 2, #8126).
	if rows := h.model.hives.Rows(); len(rows) != 2 || rows[0].Name != "other" || !rows[0].Active {
		t.Errorf("rows after the switch = %+v, want other active and first", rows)
	}
	view := h.view()
	if !strings.Contains(view, "active hive is now") || !strings.Contains(view, "restarts") {
		t.Errorf("the switch receipt does not state the restart caveat:\n%s", view)
	}
}

// Switching to the hive that is already active is a no-op that still says so,
// rather than a silently ignored keypress.
func TestHivesEnterOnTheActiveHiveSaysSo(t *testing.T) {
	h := newHivesHarness(t, seededHives())
	h.open(t)
	h.run(t, h.send(t, tea.KeyMsg{Type: tea.KeyEnter}))

	if got := h.profiles(t).Active; got != "acme" {
		t.Fatalf("active = %q, want acme unchanged", got)
	}
	if view := h.view(); !strings.Contains(view, "already active") {
		t.Errorf("re-selecting the active hive is not explained:\n%s", view)
	}
}

// Before the first successful read there is no row, so enter must not invent a
// target. Without this, the overlay's very first keypress could address a hive
// that is not on screen.
func TestHivesEnterBeforeTheListLoadsIsANoOp(t *testing.T) {
	h := newHivesHarness(t, seededHives())
	h.send(t, hivesKey) // opened, but the read is not delivered
	if cmd := h.send(t, tea.KeyMsg{Type: tea.KeyEnter}); cmd != nil {
		t.Fatal("enter issued an action before the list had loaded")
	}
}

// ── d: the typed removal confirmation ───────────────────────────────────────

// TestHivesRemoveWithoutTheTypedConfirmationIsANoOp is the acceptance
// criterion for `d`. Removing a profile discards a registration token the hub
// cannot reprint, so an unconfirmed enter must change nothing — not the file,
// not the projection, not the overlay's state.
func TestHivesRemoveWithoutTheTypedConfirmationIsANoOp(t *testing.T) {
	h := newHivesHarness(t, seededHives())
	h.open(t)

	h.run(t, h.send(t, key("d")))
	if !h.model.hives.Typing() {
		t.Fatal("d did not open the removal confirmation")
	}
	// Bare enter, then a WRONG name, then enter again. Neither may remove.
	if cmd := h.send(t, tea.KeyMsg{Type: tea.KeyEnter}); cmd != nil {
		t.Fatal("enter removed a hive with an empty confirmation")
	}
	for _, r := range "other" {
		h.send(t, key(string(r)))
	}
	if cmd := h.send(t, tea.KeyMsg{Type: tea.KeyEnter}); cmd != nil {
		t.Fatal("enter removed a hive when the typed name named a DIFFERENT one")
	}

	set := h.profiles(t)
	if len(set.Profiles) != 2 {
		t.Fatalf("profiles = %d, want both still present", len(set.Profiles))
	}
	if _, err := os.Stat(filepath.Join(h.dir, "contributor.env")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("an unconfirmed removal regenerated contributor.env (%v)", err)
	}
}

// The exact name does remove it, and the re-elected active hive is recorded —
// the rule that makes the CLI and the pane agree about what a set with a
// removed active profile means.
func TestHivesRemoveWithTheTypedNameRemovesAndReElects(t *testing.T) {
	h := newHivesHarness(t, seededHives())
	h.open(t)

	h.run(t, h.send(t, key("d")))
	for _, r := range "acme" {
		h.send(t, key(string(r)))
	}
	h.run(t, h.send(t, tea.KeyMsg{Type: tea.KeyEnter}))

	set := h.profiles(t)
	if len(set.Profiles) != 1 || set.Profiles[0].Name != "other" {
		t.Fatalf("profiles = %+v, want only other", set.Profiles)
	}
	if set.Active != "other" {
		t.Errorf("active = %q, want the surviving hive", set.Active)
	}
	if view := h.view(); !strings.Contains(view, "removed hive") {
		t.Errorf("the removal receipt is missing:\n%s", view)
	}
}

// esc backs out of the confirmation without removing anything, so a `d` pressed
// by mistake costs one key.
func TestHivesRemoveEscapesBackToTheList(t *testing.T) {
	h := newHivesHarness(t, seededHives())
	h.open(t)
	h.run(t, h.send(t, key("d")))
	h.send(t, tea.KeyMsg{Type: tea.KeyEsc})
	if h.model.hives == nil {
		t.Fatal("esc closed the whole overlay instead of the confirmation")
	}
	if h.model.hives.Typing() {
		t.Error("esc did not leave the confirmation")
	}
	if len(h.profiles(t).Profiles) != 2 {
		t.Error("escaping the confirmation removed a hive")
	}
}

// ── the probe column ────────────────────────────────────────────────────────

// TestHivesProbeFailureRendersNoAndDoesNotBlock is the fourth acceptance
// criterion. The list is drawn from the profile read alone; probes arrive
// afterwards, per hub, and a hub that does not answer renders "no" without
// having held anything up.
func TestHivesProbeFailureRendersNoAndDoesNotBlock(t *testing.T) {
	h := newHivesHarness(t, seededHives())
	h.probeErr["wss://other.example/contribute"] = true

	// The read alone, with no probe delivered: the rows are already on screen,
	// which is what "does not block" means.
	loadCmd := h.send(t, hivesKey)
	msg := loadCmd()
	loaded, ok := msg.(hivesLoadedMsg)
	if !ok {
		t.Fatalf("opening produced %T, want hivesLoadedMsg", msg)
	}
	probeCmd := h.send(t, loaded)
	if view := h.view(); !strings.Contains(view, "acme") || !strings.Contains(view, "checking…") {
		t.Fatalf("the list did not render before the probes answered:\n%s", view)
	}

	h.run(t, probeCmd)
	rows := h.model.hives.Rows()
	if len(rows) != 2 || rows[0].Reachable == nil || !*rows[0].Reachable {
		t.Fatalf("the reachable hub was not marked reachable: %+v", rows)
	}
	if rows[1].Reachable == nil || *rows[1].Reachable {
		t.Fatalf("the unreachable hub was not marked unreachable: %+v", rows)
	}
	view := h.view()
	if !strings.Contains(view, "yes") || !strings.Contains(view, "no") {
		t.Errorf("the reachable column does not render yes/no:\n%s", view)
	}
	if strings.Contains(view, "checking…") {
		t.Errorf("a settled probe still renders as pending:\n%s", view)
	}
}

// Two named sessions against one hive are two profiles with one hub. Probing it
// twice would double the traffic for one answer, and both rows must still get
// the result.
func TestHivesProbesAreDeDuplicatedByHub(t *testing.T) {
	set := &hivectl.ProfileSet{
		Version: hivectl.ProfilesVersion,
		Active:  "acme",
		Profiles: []hivectl.Profile{
			{Name: "acme", Hub: "wss://acme.example/contribute", RegistrationToken: "t1"},
			{Name: "acme-review", Hub: "wss://acme.example/contribute", RegistrationToken: "t2", Session: "review"},
		},
	}
	h := newHivesHarness(t, set)
	h.open(t)

	if got := h.probes.Load(); got != 1 {
		t.Errorf("probed %d times, want 1 for one distinct hub", got)
	}
	for i, row := range h.model.hives.Rows() {
		if row.Reachable == nil || !*row.Reachable {
			t.Errorf("row %d did not receive the shared hub's answer: %+v", i, row)
		}
	}
}

// A probe outstanding when the list changes must not resurrect a row or land on
// the wrong one.
func TestHivesProbeForAnUnknownHubIsDropped(t *testing.T) {
	h := newHivesHarness(t, seededHives())
	h.open(t)
	before := h.model.hives.Rows()
	h.send(t, hivesProbeMsg{overlayID: h.model.hivesID, hub: "wss://gone.example/contribute", reachable: true})
	if got := len(h.model.hives.Rows()); got != len(before) {
		t.Errorf("a probe for an unlisted hub changed the row count: %d -> %d", len(before), got)
	}
}

// ── a: add, and r: rename ───────────────────────────────────────────────────

// TestHivesAddRegistersAndAppends walks the add form: two fields, one
// registration POST, one appended profile carrying what the hub returned.
func TestHivesAddRegistersAndAppends(t *testing.T) {
	h := newHivesHarness(t, seededHives())
	h.open(t)

	h.run(t, h.send(t, key("a")))
	if !h.model.hives.Typing() {
		t.Fatal("a did not open the add form")
	}
	for _, r := range "third" {
		h.send(t, key(string(r)))
	}
	h.send(t, tea.KeyMsg{Type: tea.KeyTab})
	for _, r := range "wss://third.example/contribute" {
		h.send(t, key(string(r)))
	}
	h.run(t, h.send(t, tea.KeyMsg{Type: tea.KeyEnter}))

	if got := h.logins.Load(); got != 1 {
		t.Errorf("gh login resolved %d times, want 1", got)
	}
	if got := h.regs.Load(); got != 1 {
		t.Errorf("registered %d times, want 1", got)
	}
	set := h.profiles(t)
	added, _ := set.Find("third")
	if added == nil {
		t.Fatalf("the hive was not appended: %+v", set.Profiles)
	}
	if added.Hub != "wss://third.example/contribute" || added.RegistrationToken != "tok-new" || added.ContributorID != "contrib_new" {
		t.Errorf("appended profile = %+v, want the hub's answer recorded", *added)
	}
	// Adding does not steal the active marker: an operator adding a second hive
	// is not asking to switch to it.
	if set.Active != "acme" {
		t.Errorf("active = %q, want acme unchanged by an add", set.Active)
	}
}

// An add form submitted with either field empty is refused in the overlay,
// before any network call — a hub cannot be guessed and an unnamed profile is
// the positional file this feature replaced.
func TestHivesAddRequiresBothFields(t *testing.T) {
	h := newHivesHarness(t, seededHives())
	h.open(t)
	h.run(t, h.send(t, key("a")))
	for _, r := range "third" {
		h.send(t, key(string(r)))
	}
	if cmd := h.send(t, tea.KeyMsg{Type: tea.KeyEnter}); cmd != nil {
		t.Fatal("the add form submitted without a hub")
	}
	if h.regs.Load() != 0 {
		t.Error("an incomplete add form reached the hub")
	}
	if view := h.view(); !strings.Contains(view, "name and a hub URL are both required") {
		t.Errorf("the missing field is not explained:\n%s", view)
	}
}

// The hub's "already registered" answer is correct and deliberate (register is
// unauthenticated, so it must never hand an existing contributor's token to
// whoever asks). The overlay has to name the way forward rather than report a
// bare failure — and must not write a profile with no credential in it.
func TestHivesAddSurfacesTheAlreadyRegisteredPath(t *testing.T) {
	h := newHivesHarness(t, seededHives())
	h.regFn = func(context.Context, string, string) (hivectl.Registration, error) {
		return hivectl.Registration{Message: "already registered"}, nil
	}
	h.open(t)
	h.run(t, h.send(t, key("a")))
	for _, r := range "third" {
		h.send(t, key(string(r)))
	}
	h.send(t, tea.KeyMsg{Type: tea.KeyTab})
	for _, r := range "wss://third.example/contribute" {
		h.send(t, key(string(r)))
	}
	h.run(t, h.send(t, tea.KeyMsg{Type: tea.KeyEnter}))

	view := h.view()
	if !strings.Contains(view, "--token-stdin") || !strings.Contains(view, "contribute-move") {
		t.Errorf("the already-registered error does not name either way forward:\n%s", view)
	}
	if _, found := h.profiles(t).Find("third"); found >= 0 {
		t.Error("a profile was written with no registration token")
	}
	// The form stays open holding what was typed, so the operator can correct
	// the hub rather than retyping both fields.
	if !h.model.hives.Typing() {
		t.Error("a failed add bounced out of the form")
	}
}

func TestHivesRenameKeepsTheActiveMarker(t *testing.T) {
	h := newHivesHarness(t, seededHives())
	h.open(t)

	h.run(t, h.send(t, key("r")))
	// The field is prefilled with the current name; clear it and retype.
	for range "acme" {
		h.send(t, tea.KeyMsg{Type: tea.KeyBackspace})
	}
	for _, r := range "acme-prod" {
		h.send(t, key(string(r)))
	}
	h.run(t, h.send(t, tea.KeyMsg{Type: tea.KeyEnter}))

	set := h.profiles(t)
	if _, found := set.Find("acme-prod"); found < 0 {
		t.Fatalf("the hive was not renamed: %+v", set.Profiles)
	}
	if set.Active != "acme-prod" {
		t.Errorf("active = %q, want the rename carried through", set.Active)
	}
	if got := h.env(t); !strings.HasPrefix(firstHubLine(got), "HIVE_HUB=wss://acme.example/contribute") {
		t.Errorf("the projection lost its ordering after a rename:\n%s", got)
	}
}

func firstHubLine(env string) string {
	for _, line := range strings.Split(env, "\n") {
		if strings.HasPrefix(line, "HIVE_HUB=") {
			return line
		}
	}
	return ""
}

// ── modal containment ───────────────────────────────────────────────────────

// TestHivesOverlayConsumesEveryKey is the containment property. While the
// overlay is open, no key may reach the frame underneath — and while a form is
// COMPOSING, ordinary letters are text, so `p`, `q`, `a` and `K` must end up in
// the field rather than pausing an agent, quitting, or opening a second form.
func TestHivesOverlayConsumesEveryKey(t *testing.T) {
	h := newHivesHarness(t, seededHives())
	h.open(t)

	// On the list, the letters that are not bindings do nothing at all.
	for _, k := range []string{"q", "p", "m", "K", "A", "?", "x"} {
		if cmd := h.send(t, key(k)); cmd != nil {
			t.Errorf("%q produced a command while the Hives overlay was open", k)
		}
		if h.model.hives == nil {
			t.Fatalf("%q closed the Hives overlay", k)
		}
		if h.model.confirm != nil || h.model.picker != nil || h.model.acmm != nil || h.model.helpVisible {
			t.Fatalf("%q opened another overlay from inside the Hives overlay", k)
		}
	}

	// ctrl+c is the one key an operator expects to survive anything — and it
	// does not here, deliberately: every other overlay in this TUI swallows it
	// too, and esc is one key away.
	if cmd := h.send(t, tea.KeyMsg{Type: tea.KeyCtrlC}); cmd != nil {
		t.Error("ctrl+c leaked out of the Hives overlay")
	}

	// In a form, those same letters are characters.
	h.run(t, h.send(t, key("a")))
	for _, r := range "qpaK" {
		h.send(t, key(string(r)))
	}
	if h.model.hives == nil || !h.model.hives.Typing() {
		t.Fatal("typing letters into the add form closed or left it")
	}
	if !strings.Contains(h.view(), "[qpaK]") {
		t.Errorf("the add form did not receive the typed letters:\n%s", h.view())
	}
}

// Function and control keys are swallowed rather than typed: a field that
// silently accumulates invisible characters looks right and will not validate.
func TestHivesFormIgnoresNonRuneKeys(t *testing.T) {
	h := newHivesHarness(t, seededHives())
	h.open(t)
	h.run(t, h.send(t, key("r")))
	h.send(t, tea.KeyMsg{Type: tea.KeyF5})
	h.send(t, tea.KeyMsg{Type: tea.KeyCtrlW})
	h.send(t, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("r"), Alt: true})
	if view := h.view(); !strings.Contains(view, "[acme]") {
		t.Errorf("a non-rune key was typed into the rename field:\n%s", view)
	}
}

// A second enter while a write is in flight must not run it twice: the profile
// file is being rewritten and a duplicated mutation is a duplicated write.
func TestHivesRefusesASecondActionWhileOneIsPending(t *testing.T) {
	h := newHivesHarness(t, seededHives())
	h.open(t)
	h.send(t, key("j"))
	if cmd := h.send(t, tea.KeyMsg{Type: tea.KeyEnter}); cmd == nil {
		t.Fatal("the first enter did not start an action")
	}
	if !h.model.hives.Pending() {
		t.Fatal("the overlay is not marked pending after accepting an action")
	}
	for _, k := range []tea.KeyMsg{{Type: tea.KeyEnter}, {Type: tea.KeyEsc}, key("d")} {
		if cmd := h.send(t, k); cmd != nil {
			t.Errorf("%v was accepted while an action was pending", k)
		}
		if h.model.hives == nil {
			t.Fatalf("%v closed the overlay while an action was pending", k)
		}
	}
}

// ── generations ─────────────────────────────────────────────────────────────

// A read, probe or write belonging to an overlay the operator has closed and
// reopened must not populate the new one.
func TestHivesDropsAnswersForSupersededOverlays(t *testing.T) {
	h := newHivesHarness(t, seededHives())
	h.open(t)
	stale := h.model.hivesID

	h.send(t, tea.KeyMsg{Type: tea.KeyEsc})
	if h.model.hives != nil {
		t.Fatal("esc did not close the overlay")
	}
	h.open(t)
	if h.model.hivesID == stale {
		t.Fatal("reopening did not mint a new overlay id")
	}

	h.send(t, hivesLoadedMsg{overlayID: stale, err: errors.New("stale failure")})
	h.send(t, hivesActionMsg{overlayID: stale, err: errors.New("stale write")})
	if view := h.view(); strings.Contains(view, "stale") {
		t.Errorf("an answer for a superseded overlay reached the frame:\n%s", view)
	}
}

// ── failure surfaces ────────────────────────────────────────────────────────

// A store that could not be built at all (no home directory) is reported when
// the overlay opens rather than at construction: the dashboard panes have
// nothing to do with contributor profiles, and taking the whole TUI down for
// this would be a much worse trade.
func TestHivesWithoutAStoreReportsItInTheOverlay(t *testing.T) {
	h := newHivesHarness(t, seededHives())
	h.model.hivesEnv.store = nil
	h.open(t)
	if view := h.view(); !strings.Contains(view, "~/.config/hive") {
		t.Errorf("a missing config directory is not explained:\n%s", view)
	}
	if h.model.hives == nil {
		t.Fatal("the overlay closed itself on a store failure")
	}
}

// A corrupt profiles.yml is refused by the store rather than read as "no
// hives", and the refusal has to reach the operator: silently showing an empty
// list above an `a add` hint would invite them to write over a file that holds
// credentials the hub cannot reprint.
func TestHivesSurfacesAnUnreadableProfilesFile(t *testing.T) {
	h := newHivesHarness(t, nil)
	if err := os.WriteFile(filepath.Join(h.dir, "profiles.yml"), []byte("version: 1\nprofiles: not-a-list\n"), 0o600); err != nil {
		t.Fatalf("write corrupt profiles.yml: %v", err)
	}
	h.open(t)
	view := h.view()
	if !strings.Contains(view, "profiles.yml") {
		t.Errorf("the unreadable file is not named:\n%s", view)
	}
	if strings.Contains(view, "no hives configured") {
		t.Errorf("a corrupt file was rendered as an empty list:\n%s", view)
	}
}

// The overlay is raised over the frame rather than taking rows from it, so it
// must not grow the terminal — the same property every other overlay is pinned
// on.
func TestHivesOverlayDoesNotGrowTheFrame(t *testing.T) {
	h := newHivesHarness(t, seededHives())
	h.open(t)
	lines := strings.Split(h.view(), "\n")
	if len(lines) != h.model.height {
		t.Fatalf("the Hives overlay renders %d lines, want %d", len(lines), h.model.height)
	}
}

// The default wiring is what an operator actually gets. It is exercised against
// a throwaway HOME so the production path is covered without touching real
// credentials or the network.
func TestDefaultHivesEnvWiresTheProductionPieces(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	env := defaultHivesEnv()
	if env.store == nil || env.probe == nil || env.login == nil || env.register == nil {
		t.Fatalf("defaultHivesEnv left a dependency nil: %+v", env)
	}
	if !strings.HasSuffix(env.store.Path(), filepath.Join(".config", "hive", "profiles.yml")) {
		t.Errorf("store path = %q, want ~/.config/hive/profiles.yml", env.store.Path())
	}
	// The probe is the shared hivectl one: an undialable hub answers false
	// rather than hanging or panicking.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if env.probe(ctx, "wss://127.0.0.1:1/contribute") {
		t.Error("an undialable hub probed reachable")
	}
}

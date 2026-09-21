package tui

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/hivecommons/hive/pkg/hivectl"
	"github.com/hivecommons/hive/pkg/tui/panes"
)

// The Hives overlay's app-side half (#8128, phase 4 of #8097).
//
// Everything else the TUI shows arrives over the dashboard API of ONE hive.
// This does not: the hives a contributor lends a CLI to live in that
// contributor's own files, so this overlay's data source is pkg/hivectl — the
// SAME package `hivectl hives` reads and writes them through, down to the
// individual functions. Nothing here parses profiles.yml, orders the projection
// or decides which hive is active; it calls LoadOrMigrate, Use/Add/Rename/Remove
// and Commit, exactly as the CLI does, and renders what they return.
//
// pkg/tui can import pkg/hivectl (which imports nothing else of Hive's) but
// NOT pkg/hivectl/commands, which imports pkg/tui for the `tui` subcommand.
// That constraint is why the shared operations live on the store and the set
// rather than in the cobra command bodies they used to.

// hivesProbeTimeout bounds one hub reachability probe.
//
// It is shorter than the CLI's default because this one runs while an operator
// watches a rendered column rather than a shell waiting to print a table: a
// dead hive should settle on "no" in a few seconds, not sit on "checking…" for
// ten. Nothing waits on the probes either way — the list renders before any of
// them answer.
const hivesProbeTimeout = 5 * time.Second

// hivesStore is the slice of hivectl.ProfileStore this file needs, named as an
// interface so tests can point the overlay at a temporary config directory (or
// at a store that fails) without a real $HOME. *hivectl.ProfileStore satisfies
// it; nothing else implements it in production.
type hivesStore interface {
	LoadOrMigrate() (*hivectl.ProfileSet, bool, error)
	Commit(*hivectl.ProfileSet) error
	Path() string
	EnvPath() string
}

// hivesEnv is the overlay's whole dependency set: where the profiles live, and
// the two network calls adding one needs.
//
// It is a struct of function values rather than three separate model fields so
// a test replaces the lot in one assignment, and so the production wiring is
// one literal that can be read against the CLI's defaultHivesDeps.
type hivesEnv struct {
	store    hivesStore
	probe    func(ctx context.Context, hub string) bool
	login    func(ctx context.Context) (string, error)
	register func(ctx context.Context, hubHTTPBase, githubUser string) (hivectl.Registration, error)
}

// defaultHivesEnv wires the production implementations.
//
// A store that cannot be built — no resolvable home directory — leaves store
// nil rather than failing the TUI's construction: the dashboard panes have
// nothing to do with contributor profiles, and refusing to start the whole
// program over a missing $HOME would take the fleet view down with it. The
// overlay reports it when opened; see loadHives.
func defaultHivesEnv() hivesEnv {
	env := hivesEnv{
		probe: func(ctx context.Context, hub string) bool {
			return hivectl.ProbeHub(ctx, hub, hivesProbeTimeout)
		},
		login: hivectl.GitHubLogin,
		register: func(ctx context.Context, base, user string) (hivectl.Registration, error) {
			return hivectl.Register(ctx, base, user, 0)
		},
	}
	if store, err := hivectl.DefaultProfileStore(); err == nil {
		env.store = store
	}
	return env
}

// errNoHivesStore is what the overlay reports when the config directory could
// not be located at all.
var errNoHivesStore = errors.New("cannot locate ~/.config/hive: no home directory for this user")

// The Hives overlay's messages. Each carries the overlayID it was issued for,
// so a read, a probe or a write belonging to an overlay the operator has
// already closed — or closed and reopened — cannot populate the newer one with
// the older one's answer. Same generation guard as the model picker and the
// ACMM overlay.
type (
	// hivesLoadedMsg is a profile read, successful or not.
	hivesLoadedMsg struct {
		overlayID uint64
		rows      []panes.HiveRow
		path      string
		envPath   string
		err       error
	}

	// hivesProbeMsg is one hub's reachability answer. It names the HUB rather
	// than a row index because probes answer out of order and the list may have
	// been reloaded by a mutation in between.
	hivesProbeMsg struct {
		overlayID uint64
		hub       string
		reachable bool
	}

	// hivesActionMsg is a completed mutation. note is the receipt to hold on
	// the list; err is a failure to render on the form it came from.
	hivesActionMsg struct {
		overlayID uint64
		note      string
		err       error
	}
)

// openHives raises the overlay and issues the profile read.
func (m model) openHives() (model, tea.Cmd) {
	m.hivesSeq++
	m.hivesID = m.hivesSeq
	overlay := panes.NewHivesOverlay()
	m.hives = &overlay
	m.footerStatus = ""
	return m, m.loadHives(m.hivesID)
}

// updateHives consumes EVERY key while the overlay is open.
//
// The consumption rule is as strict as the ACMM overlay's, and for the same
// reason plus one: two of this overlay's screens are TEXT FIELDS, and hub URLs
// and hive names contain `a`, `d`, `q` and `p`. A key leaking through while the
// add form is open would fail to type AND pause an agent, open a second
// overlay, or quit the program. Every branch below therefore ends here, and the
// default case types or does nothing rather than routing to a pane.
func (m model) updateHives(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.hives.Pending() {
		// A write is already with the filesystem. Nothing is accepted until it
		// answers — not enter (which would run it twice), not esc (which would
		// hide a result that has already happened).
		return m, nil
	}

	// While a form is composing, ordinary letters are TEXT. This is checked
	// before the bindings below so `a`, `d` and `r` are typeable inside a hive
	// name, exactly as they are inside an ACMM confirmation phrase.
	if m.hives.Typing() {
		switch msg.String() {
		case "esc":
			next := m.hives.Cancel()
			m.hives = &next
			return m, nil
		case "enter":
			return m.submitHives()
		case "backspace":
			next := m.hives.Backspace()
			m.hives = &next
			return m, nil
		case "tab", "down":
			next := m.hives.NextField(1)
			m.hives = &next
			return m, nil
		case "shift+tab", "up":
			next := m.hives.NextField(-1)
			m.hives = &next
			return m, nil
		default:
			return m.hivesType(msg)
		}
	}

	switch msg.String() {
	case "esc":
		m.hives = nil
		return m, nil
	case "enter":
		return m.submitHives()
	case "j", "down":
		next := m.hives.Move(1)
		m.hives = &next
		return m, nil
	case "k", "up":
		next := m.hives.Move(-1)
		m.hives = &next
		return m, nil
	case "a":
		next, _ := m.hives.BeginAdd()
		m.hives = &next
		return m, nil
	case "d":
		// Opening the confirmation is ALL `d` does. The removal itself needs
		// the hive's name typed and a second enter; see HivesOverlay.Submit.
		next, _ := m.hives.BeginRemove()
		m.hives = &next
		return m, nil
	case "r":
		next, _ := m.hives.BeginRename()
		m.hives = &next
		return m, nil
	default:
		return m, nil
	}
}

// hivesType feeds a key press to the focused field.
//
// Only genuine RUNE presses become text. A function or control key — f5,
// ctrl+w, the arrows — is swallowed rather than rendered into the field,
// because no hive name or hub URL contains one and letting them accumulate
// invisibly would leave an operator staring at a field that looks right and
// will not validate.
func (m model) hivesType(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if msg.Type != tea.KeyRunes && msg.Type != tea.KeySpace {
		return m, nil
	}
	if msg.Alt {
		// alt+r is not the letter r. Treating it as one would silently insert
		// characters the operator did not type.
		return m, nil
	}
	text := string(msg.Runes)
	if msg.Type == tea.KeySpace {
		text = " "
	}
	next := m.hives.Type(text)
	m.hives = &next
	return m, nil
}

// submitHives accepts the current screen, if it is acceptable.
//
// Submit's refusals are the gates: nothing selected, an empty add form, and —
// the one that matters most — a removal confirmation whose typed name does not
// match. All of them come back ok=false and leave the overlay exactly where it
// was, so the keypress is a no-op rather than a write.
func (m model) submitHives() (tea.Model, tea.Cmd) {
	next, action, ok := m.hives.Submit()
	m.hives = &next
	if !ok {
		return m, nil
	}
	return m, m.runHivesAction(m.hivesID, action)
}

// loadHives reads the profile set and projects the rows the overlay draws.
//
// It goes through LoadOrMigrate, not Load, for the same reason every `hivectl
// hives` command does: a contributor whose machine still has the old positional
// contributor.env gets it converted on first use, named from the hub hosts,
// with the active hub preserved so a running relay is unaffected. Opening the
// TUI's pane is as good a first use as running the command.
//
// The registration tokens the set carries are NOT projected into the rows. They
// never leave pkg/hivectl's structs, so no frame, footer or golden file in this
// package can ever render one.
func (m model) loadHives(overlayID uint64) tea.Cmd {
	env := m.hivesEnv
	return func() tea.Msg {
		if env.store == nil {
			return hivesLoadedMsg{overlayID: overlayID, err: errNoHivesStore}
		}
		set, _, err := env.store.LoadOrMigrate()
		if errors.Is(err, hivectl.ErrNoProfiles) {
			// Not a failure: this machine has never run contribute-setup. The
			// CLI turns it into setup guidance because a command with nothing
			// to act on has nothing else to do; the overlay renders the empty
			// list instead, because `a` is right there and adding the first
			// hive is exactly what an operator opened this for.
			err = nil
			set = nil
		}
		if err != nil {
			return hivesLoadedMsg{overlayID: overlayID, err: err, path: env.store.Path(), envPath: env.store.EnvPath()}
		}
		return hivesLoadedMsg{
			overlayID: overlayID,
			rows:      hiveRows(set),
			path:      env.store.Path(),
			envPath:   env.store.EnvPath(),
		}
	}
}

// hiveRows projects a profile set into the overlay's rows, in the order the
// relay will walk them (active first) — the same order `hivectl hives list`
// prints, so the two surfaces never disagree about which hive comes first.
func hiveRows(set *hivectl.ProfileSet) []panes.HiveRow {
	if set == nil {
		return nil
	}
	active := set.ActiveProfile()
	ordered := set.Ordered()
	rows := make([]panes.HiveRow, 0, len(ordered))
	for _, p := range ordered {
		rows = append(rows, panes.HiveRow{
			Name:          p.Name,
			Hub:           p.Hub,
			ContributorID: p.ContributorID,
			Session:       p.Session,
			Active:        active != nil && strings.EqualFold(active.Name, p.Name),
		})
	}
	return rows
}

// probeHives issues one probe per DISTINCT hub.
//
// One Cmd each rather than one Cmd for all of them: each answers as it lands,
// so a hive that is up shows "yes" while a hive that is down is still timing
// out. That is the whole reason the column cannot block the pane — there is no
// point at which the frame waits for a probe, and the slowest hub costs the
// others nothing.
//
// De-duplicated by hub because two named sessions against one hive are two
// profiles with the same URL, and probing it twice would double the traffic for
// one answer that SetReachable fans out to both rows anyway.
func (m model) probeHives(overlayID uint64, rows []panes.HiveRow) tea.Cmd {
	env := m.hivesEnv
	if env.probe == nil {
		return nil
	}
	seen := make(map[string]struct{}, len(rows))
	var cmds []tea.Cmd
	for _, row := range rows {
		hub := row.Hub
		if hub == "" {
			continue
		}
		if _, dup := seen[hub]; dup {
			continue
		}
		seen[hub] = struct{}{}
		cmds = append(cmds, func() tea.Msg {
			return hivesProbeMsg{overlayID: overlayID, hub: hub, reachable: env.probe(context.Background(), hub)}
		})
	}
	if len(cmds) == 0 {
		return nil
	}
	return tea.Batch(cmds...)
}

// runHivesAction performs one mutation and returns its receipt.
//
// It RELOADS the set before mutating rather than acting on the snapshot the
// overlay is drawing. The profiles file is shared with `hivectl hives` and with
// any other terminal the operator has open, so the rows on screen are a
// reading, not a lock — and applying a rename to a set loaded a minute ago
// would silently revert whatever happened in between.
func (m model) runHivesAction(overlayID uint64, action panes.HivesAction) tea.Cmd {
	env := m.hivesEnv
	return func() tea.Msg {
		if env.store == nil {
			return hivesActionMsg{overlayID: overlayID, err: errNoHivesStore}
		}
		set, _, err := env.store.LoadOrMigrate()
		if err != nil && !(errors.Is(err, hivectl.ErrNoProfiles) && action.Kind == panes.HivesActionAdd) {
			return hivesActionMsg{overlayID: overlayID, err: err}
		}
		if set == nil {
			// Only reachable for an add on a machine with nothing configured,
			// which is the one action that is meaningful there.
			set = &hivectl.ProfileSet{Version: hivectl.ProfilesVersion}
		}
		note, err := applyHivesAction(env, set, action)
		if err != nil {
			return hivesActionMsg{overlayID: overlayID, err: err}
		}
		if err := env.store.Commit(set); err != nil {
			return hivesActionMsg{overlayID: overlayID, err: err}
		}
		return hivesActionMsg{overlayID: overlayID, note: note}
	}
}

// applyHivesAction mutates the set in memory and returns the receipt to render.
//
// NOTHING is written here: the caller commits, once, after this returns. That
// is what keeps "profiles.yml before its projection" a property of
// ProfileStore.Commit rather than something each of the four actions has to
// remember.
func applyHivesAction(env hivesEnv, set *hivectl.ProfileSet, action panes.HivesAction) (string, error) {
	switch action.Kind {
	case panes.HivesActionUse:
		profile, already, err := set.Use(action.Name)
		if err != nil {
			return "", err
		}
		if already {
			return fmt.Sprintf("✓ %q was already active (%s)", profile.Name, profile.Hub), nil
		}
		return panes.HivesUseNote(profile.Name, profile.Hub, env.store.EnvPath()), nil

	case panes.HivesActionAdd:
		profile, err := registerHive(env, action.Name, action.Hub)
		if err != nil {
			return "", err
		}
		if err := set.Add(profile, false); err != nil {
			return "", err
		}
		note := fmt.Sprintf("✓ added hive %q (%s)", profile.Name, profile.Hub)
		if profile.ContributorID != "" {
			note += " as " + profile.ContributorID
		}
		return note, nil

	case panes.HivesActionRemove:
		removed, wasActive, err := set.Remove(action.Name)
		if err != nil {
			return "", err
		}
		note := fmt.Sprintf("✓ removed hive %q (%s); previous credentials kept at %s.bak",
			removed.Name, removed.Hub, env.store.EnvPath())
		switch {
		case len(set.Profiles) == 0:
			note += " — no hives left"
		case wasActive:
			note += fmt.Sprintf(" — active hive is now %q", set.Active)
		}
		return note, nil

	case panes.HivesActionRename:
		profile, err := set.Rename(action.Name, action.NewName)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("✓ renamed hive %q to %q (%s)", action.Name, profile.Name, profile.Hub), nil
	}
	return "", fmt.Errorf("unsupported hives action %d", action.Kind)
}

// registerHive performs the registration half of `just contribute-setup` for
// the add form: resolve the GitHub login, POST to the hub, keep what it
// returns.
//
// The hub's "already registered" answer — a 2xx with no token — is NOT a
// failure of this code and is not presented as one. Register is unauthenticated
// and must never hand an existing contributor's credential to whoever asks
// (#4408), so the refusal is correct; what the operator needs is the way
// forward, which is the CLI's --token-stdin path or `just contribute-move`.
// Neither is a keyboard flow, so the overlay names them rather than pretending
// to offer them.
func registerHive(env hivesEnv, name, hub string) (hivectl.Profile, error) {
	if err := hivectl.ValidateProfileName(name); err != nil {
		return hivectl.Profile{}, err
	}
	if err := hivectl.ValidateHubURL(hub); err != nil {
		return hivectl.Profile{}, err
	}
	base, err := hivectl.HubHTTPBase(hub)
	if err != nil {
		return hivectl.Profile{}, err
	}
	user, err := env.login(context.Background())
	if err != nil {
		return hivectl.Profile{}, err
	}
	reg, err := env.register(context.Background(), base, user)
	if err != nil {
		return hivectl.Profile{}, err
	}
	token := strings.TrimSpace(reg.RegistrationToken)
	if token == "" {
		message := strings.TrimSpace(reg.Message)
		if message == "" {
			message = "the hub returned no registration token"
		}
		return hivectl.Profile{}, fmt.Errorf(
			"%s on %s: %s — register is unauthenticated, so the hub will never hand an existing contributor's token back. "+
				"Add the credential you already hold from a shell: "+
				"printf '%%s' \"$TOKEN\" | hivectl hives add %s --hub %s --token-stdin --contributor-id <id>, "+
				"or move the identity with 'just contribute-move'",
			user, base, message, name, hub)
	}
	return hivectl.Profile{
		Name:              name,
		Hub:               hub,
		ContributorID:     strings.TrimSpace(reg.ContributorID),
		RegistrationToken: token,
		AddedAt:           time.Now().UTC().Truncate(time.Second),
	}, nil
}

func (m model) handleHivesLoaded(msg hivesLoadedMsg) (tea.Model, tea.Cmd) {
	// A read for a closed or superseded overlay is dropped: it describes a set
	// the current overlay may already have changed.
	if m.hives == nil || m.hivesID != msg.overlayID {
		return m, nil
	}
	if msg.err != nil {
		next := m.hives.SetListError(msg.err)
		m.hives = &next
		return m, nil
	}
	next := m.hives.SetHives(msg.rows, msg.path, msg.envPath)
	m.hives = &next
	// Probing starts only once there is a list to probe, and the list is
	// already on screen by then. Nothing below waits for it.
	return m, m.probeHives(msg.overlayID, msg.rows)
}

func (m model) handleHivesProbe(msg hivesProbeMsg) (tea.Model, tea.Cmd) {
	if m.hives == nil || m.hivesID != msg.overlayID {
		return m, nil
	}
	next := m.hives.SetReachable(msg.hub, msg.reachable)
	m.hives = &next
	return m, nil
}

func (m model) handleHivesAction(msg hivesActionMsg) (tea.Model, tea.Cmd) {
	if m.hives == nil || m.hivesID != msg.overlayID {
		return m, nil
	}
	if msg.err != nil {
		next := m.hives.SetActionError(msg.err)
		m.hives = &next
		return m, nil
	}
	next := m.hives.SetActionResult(msg.note)
	m.hives = &next
	// Reload rather than patching the rows in place: the file is the truth, it
	// was just rewritten, and the reload is what makes the active marker and
	// the projection order on screen agree with what the relay will read.
	return m, m.loadHives(msg.overlayID)
}

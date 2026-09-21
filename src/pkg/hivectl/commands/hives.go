package commands

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/hivectl"
	"github.com/spf13/cobra"
)

// `hivectl hives` — named profiles for the hives a contributor lends a CLI to
// (#8097).
//
// These subcommands are the only ones in hivectl that do not talk to a hive
// dashboard: they read and write the contributor's own credential files
// ($HOME/.config/hive/profiles.yml and the contributor.env it generates), and
// `hives add` talks to a CONTRIBUTOR HUB, which is a different endpoint on a
// different URL from --server. So they build their own store and HTTP client
// rather than going through commandEnv.client(), and --server/--token-env do
// not apply to them.
//
// The file format, the five mutations and the two hub calls all live in
// pkg/hivectl (profiles.go, profiles_ops.go, contribute.go), not here. This
// file is the CLI SURFACE over them: flags, prompts, output and the advice each
// failure comes with. The TUI's Hives pane (#8128) is a second surface over the
// same functions rather than a second implementation of any of it.

// hubRegistrar performs the registration half of `just contribute-setup`:
// POST <hub>/api/contribute/register with a GitHub username. Injected so tests
// never reach the network.
type hubRegistrar interface {
	Register(ctx context.Context, hubHTTPBase, githubUser string) (hubRegistration, error)
}

// hubRegistration is the hub's answer to a register call, as this package's
// seam sees it. It is hivectl.Registration under another name so the injected
// interface above does not force every test double to import the shared type.
type hubRegistration = hivectl.Registration

// hivesDeps are the seams `hivectl hives` is tested through.
type hivesDeps struct {
	store       *hivectl.ProfileStore
	registrar   hubRegistrar
	githubUser  func(ctx context.Context) (string, error)
	signalRelay func(ctx context.Context, store *hivectl.ProfileStore) (hivectl.RelaySwitchResult, error)
	now         func() time.Time
}

func newHivesCommand(env *commandEnv) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "hives",
		Aliases: []string{"hive"},
		Short:   "Manage the named hives you lend a CLI to",
		Long: "Save the hives you contribute to as named profiles, see which one is active, " +
			"and switch, add, rename or remove one without hand-editing ~/.config/hive/contributor.env.\n\n" +
			"Profiles live in ~/.config/hive/profiles.yml (mode 0600). contributor.env is regenerated " +
			"from it, with the active hive first, so the contributor relay keeps reading exactly the " +
			"variables it always has. A relay that is already running picks up a switch on its next start.",
		Example: `  hivectl hives list
  hivectl hives add acme --hub wss://acme.hive.hivecommons.dev/contribute
  hivectl hives use acme
  hivectl hives export acme --out acme.hive-profile
  hivectl hives import acme.hive-profile --name acme-laptop
  hivectl hives session acme --label review
  hivectl hives rename acme acme-prod
  hivectl hives remove acme`,
	}
	cmd.AddCommand(newHivesListCommand(env))
	cmd.AddCommand(newHivesAddCommand(env))
	cmd.AddCommand(newHivesUseCommand(env))
	cmd.AddCommand(newHivesExportCommand(env))
	cmd.AddCommand(newHivesImportCommand(env))
	cmd.AddCommand(newHivesSessionCommand(env))
	cmd.AddCommand(newHivesRenameCommand(env))
	cmd.AddCommand(newHivesRemoveCommand(env))
	return cmd
}

// defaultHivesDeps wires the production implementations.
func defaultHivesDeps(timeout time.Duration) (*hivesDeps, error) {
	store, err := hivectl.DefaultProfileStore()
	if err != nil {
		return nil, err
	}
	return &hivesDeps{
		store:       store,
		registrar:   httpRegistrar{timeout: timeout},
		githubUser:  hivectl.GitHubLogin,
		signalRelay: hivectl.SignalRunningRelay,
		now:         time.Now,
	}, nil
}

// hivesDepsFor lets tests substitute the whole dependency set for one command
// run. Production callers leave it nil.
var hivesDepsFor func(env *commandEnv) (*hivesDeps, error)

func (e *commandEnv) hivesDeps() (*hivesDeps, error) {
	if hivesDepsFor != nil {
		return hivesDepsFor(e)
	}
	return defaultHivesDeps(e.options.timeout)
}

// loadProfiles reads the profile set, migrating a legacy positional
// contributor.env on first use and telling the operator once that it did.
//
// allowEmpty is for `hives add`, which is the one command that is meaningful
// on a machine with nothing configured yet; every other one has nothing to act
// on and says so with setup guidance instead.
func loadProfiles(cmd *cobra.Command, deps *hivesDeps, allowEmpty bool) (*hivectl.ProfileSet, error) {
	set, migrated, err := deps.store.LoadOrMigrate()
	if errors.Is(err, hivectl.ErrNoProfiles) {
		if allowEmpty {
			return &hivectl.ProfileSet{Version: hivectl.ProfilesVersion}, nil
		}
		return nil, fmt.Errorf("no hives configured yet — run 'hivectl hives add <name> --hub <url>', or 'just contribute-setup <backend>' for the full first-time setup (looked in %s and %s)", deps.store.Path(), deps.store.EnvPath())
	}
	if err != nil {
		return nil, err
	}
	if migrated {
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "Migrated %d hive(s) from %s into %s. contributor.env is unchanged; it is regenerated from the profiles when you next add, remove, rename or switch a hive.\n",
			len(set.Profiles), deps.store.EnvPath(), deps.store.Path())
	}
	return set, nil
}

// commit saves the profiles and regenerates contributor.env from them. The
// ordering rule it depends on lives with the store (ProfileStore.Commit), so
// the TUI's pane cannot get it wrong differently.
func commit(deps *hivesDeps, set *hivectl.ProfileSet) error {
	return deps.store.Commit(set)
}

// ── list ────────────────────────────────────────────────────────────────────

type hivesListRow struct {
	Name          string `json:"name" yaml:"name"`
	Hub           string `json:"hub" yaml:"hub"`
	ContributorID string `json:"contributor_id,omitempty" yaml:"contributor_id,omitempty"`
	Session       string `json:"session,omitempty" yaml:"session,omitempty"`
	Active        bool   `json:"active" yaml:"active"`
	AddedAt       string `json:"added_at,omitempty" yaml:"added_at,omitempty"`
	LastSeen      string `json:"last_seen,omitempty" yaml:"last_seen,omitempty"`
	Reachable     *bool  `json:"reachable,omitempty" yaml:"reachable,omitempty"`
}

func newHivesListCommand(env *commandEnv) *cobra.Command {
	check := false
	cmd := &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List your hive profiles and show which one is active",
		Long: "Lists the hives in ~/.config/hive/profiles.yml in the order the relay will use them: " +
			"active first. Registration tokens are never printed, in any output format.\n\n" +
			"--check additionally asks each hub whether it is answering; it is off by default so the " +
			"list stays usable offline and never blocks on an unreachable hive.",
		Args: argsNone(),
		RunE: func(cmd *cobra.Command, _ []string) error {
			return env.runHivesList(cmd, check)
		},
	}
	cmd.Flags().BoolVar(&check, "check", false, "probe each hub's /api/contribute/status and report whether it answered")
	return cmd
}

func (e *commandEnv) runHivesList(cmd *cobra.Command, check bool) error {
	if !validOutput(e.options.output) {
		return &usageError{message: fmt.Sprintf("invalid --output %q; expected table, json, yaml, or jsonl", e.options.output)}
	}
	deps, err := e.hivesDeps()
	if err != nil {
		return err
	}
	set, err := loadProfiles(cmd, deps, false)
	if err != nil {
		return err
	}
	active := set.ActiveProfile()
	lastSeen, err := deps.store.LoadHubsSeen()
	if err != nil {
		return err
	}
	rows := make([]hivesListRow, 0, len(set.Profiles))
	for _, p := range set.Ordered() {
		row := hivesListRow{
			Name:          p.Name,
			Hub:           p.Hub,
			ContributorID: p.ContributorID,
			Session:       p.Session,
			Active:        active != nil && strings.EqualFold(active.Name, p.Name),
		}
		if !p.AddedAt.IsZero() {
			row.AddedAt = p.AddedAt.UTC().Format(time.RFC3339)
		}
		if seen, ok := lastSeen[p.Hub]; ok {
			row.LastSeen = seen.UTC().Format(time.RFC3339)
		}
		if check {
			reachable := probeHub(cmd.Context(), p.Hub, e.options.timeout)
			row.Reachable = &reachable
		}
		rows = append(rows, row)
	}
	if e.options.output != "table" {
		return e.print(cmd, rows)
	}
	printHivesTable(cmd.OutOrStdout(), rows, check)
	return nil
}

func printHivesTable(out io.Writer, rows []hivesListRow, check bool) {
	header := " \tNAME\tHUB\tCONTRIBUTOR ID\tSESSION\tADDED\tLAST SEEN"
	if check {
		header += "\tREACHABLE"
	}
	var buf bytes.Buffer
	_, _ = fmt.Fprintln(&buf, header)
	for _, row := range rows {
		marker := " "
		if row.Active {
			marker = "*"
		}
		line := fmt.Sprintf("%s\t%s\t%s\t%s\t%s\t%s\t%s", marker, row.Name, row.Hub, dashIfEmpty(row.ContributorID), dashIfEmpty(row.Session), dashIfEmpty(row.AddedAt), dashIfEmpty(row.LastSeen))
		if check {
			state := "-"
			if row.Reachable != nil {
				state = "no"
				if *row.Reachable {
					state = "yes"
				}
			}
			line += "\t" + state
		}
		_, _ = fmt.Fprintln(&buf, line)
	}
	// printer's table renderer only understands maps and slices of maps; the
	// hive list is a fixed set of columns with a leading active marker, so it
	// is laid out here and handed over as preformatted text.
	_ = printer{format: "table", out: out}.print(strings.TrimRight(buf.String(), "\n"))
	_, _ = fmt.Fprintln(out, "\n* = active: the hub the relay solicits from first.")
}

func dashIfEmpty(value string) string {
	if strings.TrimSpace(value) == "" {
		return "-"
	}
	return value
}

// probeHub reports whether the hub answered its contributor status endpoint.
// The probe itself is hivectl.ProbeHub, shared with the TUI's Hives pane.
func probeHub(ctx context.Context, hub string, timeout time.Duration) bool {
	return hivectl.ProbeHub(ctx, hub, timeout)
}

// ── add ─────────────────────────────────────────────────────────────────────

type hivesAddOptions struct {
	hub           string
	githubUser    string
	session       string
	backend       string
	model         string
	contributorID string
	tokenStdin    bool
	activate      bool
}

func newHivesAddCommand(env *commandEnv) *cobra.Command {
	opts := &hivesAddOptions{}
	cmd := &cobra.Command{
		Use:   "add <name> --hub <url>",
		Short: "Register with a hive and save it as a named profile",
		Long: "Runs the registration half of 'just contribute-setup' — POST <hub>/api/contribute/register — " +
			"and appends the result to your profiles. It does not touch gh auth or the backend CLI preflight; " +
			"use 'just contribute-setup' for a first-time machine.\n\n" +
			"If you are already registered with this hive on another machine, the hub will not hand the token " +
			"back (register is unauthenticated, so it must not). Pass --token-stdin --contributor-id <id> to add " +
			"the credential you already hold instead.",
		Example: `  hivectl hives add acme --hub wss://acme.hive.hivecommons.dev/contribute
  hivectl hives add acme-2 --hub wss://acme.hive.hivecommons.dev/contribute --session review
  printf '%s' "$TOKEN" | hivectl hives add acme --hub wss://acme.example/contribute --token-stdin --contributor-id contrib_123`,
		Args: argsExact(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return env.runHivesAdd(cmd, args[0], opts)
		},
	}
	cmd.Flags().StringVar(&opts.hub, "hub", "", "contributor hub URL, e.g. wss://<hive>/contribute (required)")
	cmd.Flags().StringVar(&opts.githubUser, "github-user", "", "GitHub login to register as (default: the account 'gh' is signed in as)")
	cmd.Flags().StringVar(&opts.session, "session", "", "session label, so two profiles for one hive run as two sessions")
	cmd.Flags().StringVar(&opts.backend, "backend", "", "backend default recorded on the profile")
	cmd.Flags().StringVar(&opts.model, "model", "", "model default recorded on the profile")
	cmd.Flags().StringVar(&opts.contributorID, "contributor-id", "", "existing contributor id, with --token-stdin")
	cmd.Flags().BoolVar(&opts.tokenStdin, "token-stdin", false, "read an existing registration token from stdin instead of registering")
	cmd.Flags().BoolVar(&opts.activate, "activate", false, "make the new hive the active one")
	return cmd
}

func (e *commandEnv) runHivesAdd(cmd *cobra.Command, name string, opts *hivesAddOptions) error {
	if err := hivectl.ValidateProfileName(name); err != nil {
		return &usageError{message: err.Error()}
	}
	hub := strings.TrimSpace(opts.hub)
	if hub == "" {
		return &usageError{message: "--hub is required: the contributor hub URL of the hive, e.g. wss://<hive>/contribute"}
	}
	if err := hivectl.ValidateHubURL(hub); err != nil {
		return &usageError{message: err.Error()}
	}
	if opts.contributorID != "" && !opts.tokenStdin {
		return &usageError{message: "--contributor-id applies only with --token-stdin; without it the hub assigns the id"}
	}
	deps, err := e.hivesDeps()
	if err != nil {
		return err
	}

	set, err := loadProfiles(cmd, deps, true)
	if err != nil {
		return err
	}
	if existing, _ := set.Find(name); existing != nil {
		return &usageError{message: fmt.Sprintf("a hive profile named %q already exists (%s); pick another name, or 'hivectl hives remove %s' first", existing.Name, existing.Hub, existing.Name)}
	}

	profile := hivectl.Profile{
		Name:          name,
		Hub:           hub,
		Session:       strings.TrimSpace(opts.session),
		Backend:       strings.TrimSpace(opts.backend),
		Model:         strings.TrimSpace(opts.model),
		ContributorID: strings.TrimSpace(opts.contributorID),
		AddedAt:       deps.now().UTC().Truncate(time.Second),
	}

	if opts.tokenStdin {
		token, err := readTokenFrom(cmd.InOrStdin())
		if err != nil {
			return err
		}
		profile.RegistrationToken = token
	} else {
		user := strings.TrimSpace(opts.githubUser)
		if user == "" {
			user, err = deps.githubUser(cmd.Context())
			if err != nil {
				return err
			}
		}
		base, err := hivectl.HubHTTPBase(hub)
		if err != nil {
			return &usageError{message: err.Error()}
		}
		reg, err := deps.registrar.Register(cmd.Context(), base, user)
		if err != nil {
			return err
		}
		if strings.TrimSpace(reg.RegistrationToken) == "" {
			return alreadyRegisteredError(user, base, reg.Message, name, hub)
		}
		profile.RegistrationToken = strings.TrimSpace(reg.RegistrationToken)
		if profile.ContributorID == "" {
			profile.ContributorID = strings.TrimSpace(reg.ContributorID)
		}
	}

	if err := set.Add(profile, opts.activate); err != nil {
		return err
	}
	if err := commit(deps, set); err != nil {
		return err
	}

	out := cmd.OutOrStdout()
	_, _ = fmt.Fprintf(out, "✓ Added hive %q (%s)\n", profile.Name, profile.Hub)
	if profile.ContributorID != "" {
		_, _ = fmt.Fprintf(out, "  contributor id: %s\n", profile.ContributorID)
	}
	if strings.EqualFold(set.Active, profile.Name) {
		_, _ = fmt.Fprintln(out, "  active: yes — the relay will solicit from this hive first")
	}
	_, _ = fmt.Fprintf(out, "  saved to %s; %s regenerated\n", deps.store.Path(), deps.store.EnvPath())
	_, _ = fmt.Fprintln(out, "  restart the relay ('just contribute-stop' then 'just contribute-hive') to pick this up")
	return nil
}

// alreadyRegisteredError turns the hub's "already registered, no token for
// you" answer into the supported way forward, rather than a bare failure.
//
// The refusal is correct and must stay: /api/contribute/register is
// unauthenticated, so handing an existing contributor's token to whoever POSTs
// their username would be an account-takeover primitive (#4408).
func alreadyRegisteredError(user, base, message, name, hub string) error {
	if message == "" {
		message = "the hub returned no registration token"
	}
	return fmt.Errorf(`%s on %s: %s

register is unauthenticated, so the hub will never hand an existing contributor's
token to whoever asks. If you already hold that credential, add it directly:

    printf '%%s' "$TOKEN" | hivectl hives add %s --hub %s \
      --token-stdin --contributor-id <your-contributor-id>

The token and id are the HIVE_REGISTRATION_TOKEN / CONTRIBUTOR_ID entries for
this hub in ~/.config/hive/contributor.env on the machine already registered.
To move the identity instead, use 'just contribute-move', which authenticates
with GitHub and can reissue it`, user, base, message, name, hub)
}

func readTokenFrom(in io.Reader) (string, error) {
	if in == nil {
		return "", &usageError{message: "--token-stdin was given but there is no stdin to read"}
	}
	data, err := io.ReadAll(io.LimitReader(in, 64*1024))
	if err != nil {
		return "", fmt.Errorf("read registration token from stdin: %w", err)
	}
	token := strings.TrimSpace(string(data))
	if token == "" {
		return "", &usageError{message: "--token-stdin was given but stdin was empty"}
	}
	return token, nil
}

func readPassphrase(in io.Reader, promptOut io.Writer, fromStdin bool) ([]byte, error) {
	if promptOut != nil {
		_, _ = fmt.Fprint(promptOut, "Profile bundle passphrase: ")
	}
	if !fromStdin && in == nil {
		return nil, &usageError{message: "no input available for the profile bundle passphrase; pass --passphrase-stdin to read it from stdin"}
	}
	line, err := bufio.NewReader(io.LimitReader(in, 64*1024)).ReadString('\n')
	if err != nil {
		if !errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("read profile bundle passphrase: %w", err)
		}
	}
	pass := strings.TrimRight(line, "\r\n")
	if strings.TrimSpace(pass) == "" {
		return nil, &usageError{message: "profile bundle passphrase must not be empty"}
	}
	return []byte(pass), nil
}

// ── use ─────────────────────────────────────────────────────────────────────

func newHivesUseCommand(env *commandEnv) *cobra.Command {
	return &cobra.Command{
		Use:     "use <name>",
		Aliases: []string{"switch"},
		Short:   "Make a hive the active one",
		Long: "Marks the named hive active and regenerates contributor.env with it first in the hub list. " +
			"If a contributor relay is already running, hivectl asks it to reload the projection with SIGUSR1 " +
			"(or docker/podman kill --signal USR1 for container mode) so the switch takes effect without a restart. " +
			"Any task already in flight finishes on the hub that assigned it; the next solicitation goes to the new active hive.",
		Args: argsExact(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return env.runHivesUse(cmd, args[0])
		},
	}
}

func (e *commandEnv) runHivesUse(cmd *cobra.Command, name string) error {
	deps, err := e.hivesDeps()
	if err != nil {
		return err
	}
	set, err := loadProfiles(cmd, deps, false)
	if err != nil {
		return err
	}
	target, already, err := set.Use(name)
	if err != nil {
		return unknownHiveError(name, set)
	}
	if err := commit(deps, set); err != nil {
		return err
	}
	switchResult := hivectl.RelaySwitchResult{}
	if deps.signalRelay != nil {
		switchResult, err = deps.signalRelay(cmd.Context(), deps.store)
		if err != nil {
			return err
		}
	}
	out := cmd.OutOrStdout()
	if already {
		_, _ = fmt.Fprintf(out, "✓ %q was already active (%s)\n", target.Name, target.Hub)
	} else {
		_, _ = fmt.Fprintf(out, "✓ Active hive is now %q (%s)\n", target.Name, target.Hub)
	}
	_, _ = fmt.Fprintf(out, "  %s regenerated with %q first\n", deps.store.EnvPath(), target.Name)
	if switchResult.Running {
		_, _ = fmt.Fprintf(out, "  running relay signaled (%s); in-flight work finishes on its original hive, then the relay solicits from %q\n", switchResult.Target, target.Name)
	} else {
		_, _ = fmt.Fprintln(out, "  no running relay found; the next relay start will solicit from this hive first")
	}
	return nil
}

// ── export/import/session ──────────────────────────────────────────────────

type hivesExportOptions struct {
	outFile         string
	passphraseStdin bool
}

func newHivesExportCommand(env *commandEnv) *cobra.Command {
	opts := &hivesExportOptions{}
	cmd := &cobra.Command{
		Use:   "export <name>",
		Short: "Export one hive profile as a passphrase-encrypted bundle",
		Long: "Writes one named profile as an AES-256-GCM encrypted bundle. The registration token is never printed in plaintext; " +
			"the bundle can be imported on another machine with 'hivectl hives import'.",
		Args: argsExact(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return env.runHivesExport(cmd, args[0], opts)
		},
	}
	cmd.Flags().StringVar(&opts.outFile, "out", "", "write the encrypted bundle to a file instead of stdout")
	cmd.Flags().BoolVar(&opts.passphraseStdin, "passphrase-stdin", false, "read the bundle passphrase from stdin")
	return cmd
}

func (e *commandEnv) runHivesExport(cmd *cobra.Command, name string, opts *hivesExportOptions) error {
	deps, err := e.hivesDeps()
	if err != nil {
		return err
	}
	set, err := loadProfiles(cmd, deps, false)
	if err != nil {
		return err
	}
	profile, _ := set.Find(name)
	if profile == nil {
		return unknownHiveError(name, set)
	}
	pass, err := readPassphrase(cmd.InOrStdin(), cmd.ErrOrStderr(), opts.passphraseStdin)
	if err != nil {
		return err
	}
	bundle, err := hivectl.EncryptProfileBundle(*profile, pass)
	if err != nil {
		return err
	}
	if opts.outFile != "" {
		if err := os.WriteFile(opts.outFile, bundle, 0o600); err != nil {
			return fmt.Errorf("write encrypted hive profile bundle %s: %w", opts.outFile, err)
		}
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "✓ Exported hive %q to %s (encrypted; token not printed)\n", profile.Name, opts.outFile)
		return nil
	}
	_, err = cmd.OutOrStdout().Write(bundle)
	return err
}

type hivesImportOptions struct {
	name            string
	activate        bool
	passphraseStdin bool
}

func newHivesImportCommand(env *commandEnv) *cobra.Command {
	opts := &hivesImportOptions{}
	cmd := &cobra.Command{
		Use:   "import <bundle-file>",
		Short: "Import a passphrase-encrypted hive profile bundle",
		Args:  argsExact(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return env.runHivesImport(cmd, args[0], opts)
		},
	}
	cmd.Flags().StringVar(&opts.name, "name", "", "save the imported profile under this name")
	cmd.Flags().BoolVar(&opts.activate, "activate", false, "make the imported hive active")
	cmd.Flags().BoolVar(&opts.passphraseStdin, "passphrase-stdin", false, "read the bundle passphrase from stdin")
	return cmd
}

func (e *commandEnv) runHivesImport(cmd *cobra.Command, bundleFile string, opts *hivesImportOptions) error {
	deps, err := e.hivesDeps()
	if err != nil {
		return err
	}
	set, err := loadProfiles(cmd, deps, true)
	if err != nil {
		return err
	}
	data, err := os.ReadFile(bundleFile)
	if err != nil {
		return fmt.Errorf("read encrypted hive profile bundle %s: %w", bundleFile, err)
	}
	pass, err := readPassphrase(cmd.InOrStdin(), cmd.ErrOrStderr(), opts.passphraseStdin)
	if err != nil {
		return err
	}
	profile, err := hivectl.DecryptProfileBundle(data, pass)
	if err != nil {
		return err
	}
	if opts.name != "" {
		if err := hivectl.ValidateProfileName(opts.name); err != nil {
			return &usageError{message: err.Error()}
		}
		profile.Name = opts.name
	}
	if existing, _ := set.Find(profile.Name); existing != nil {
		return &usageError{message: fmt.Sprintf("a hive profile named %q already exists (%s); pass --name to import it under a different name", existing.Name, existing.Hub)}
	}
	if err := set.Add(profile, opts.activate); err != nil {
		if errors.Is(err, hivectl.ErrProfileExists) {
			return &usageError{message: err.Error()}
		}
		return err
	}
	if err := commit(deps, set); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "✓ Imported hive %q (%s)\n", profile.Name, profile.Hub)
	return nil
}

type hivesSessionOptions struct {
	label    string
	name     string
	activate bool
}

func newHivesSessionCommand(env *commandEnv) *cobra.Command {
	opts := &hivesSessionOptions{}
	cmd := &cobra.Command{
		Use:   "session <name> --label <label>",
		Short: "Create a second named session profile for one hive",
		Long: "Copies an existing profile under a new name and sets its HIVE_SESSION label. " +
			"Two profiles for one hub with different session labels run as separate hub sessions.",
		Args: argsExact(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return env.runHivesSession(cmd, args[0], opts)
		},
	}
	cmd.Flags().StringVar(&opts.label, "label", "", "session label to project as HIVE_SESSION (required)")
	cmd.Flags().StringVar(&opts.name, "name", "", "name for the copied profile (default: <name>-<label>)")
	cmd.Flags().BoolVar(&opts.activate, "activate", false, "make the session profile active")
	return cmd
}

func (e *commandEnv) runHivesSession(cmd *cobra.Command, sourceName string, opts *hivesSessionOptions) error {
	label := strings.TrimSpace(opts.label)
	if label == "" {
		return &usageError{message: "--label is required"}
	}
	deps, err := e.hivesDeps()
	if err != nil {
		return err
	}
	set, err := loadProfiles(cmd, deps, false)
	if err != nil {
		return err
	}
	source, _ := set.Find(sourceName)
	if source == nil {
		return unknownHiveError(sourceName, set)
	}
	name := strings.TrimSpace(opts.name)
	if name == "" {
		name = source.Name + "-" + label
	}
	if err := hivectl.ValidateProfileName(name); err != nil {
		return &usageError{message: err.Error()}
	}
	if existing, _ := set.Find(name); existing != nil {
		return &usageError{message: fmt.Sprintf("a hive profile named %q already exists (%s)", existing.Name, existing.Hub)}
	}
	copy := *source
	copy.Name = name
	copy.Session = label
	copy.AddedAt = deps.now().UTC().Truncate(time.Second)
	if err := set.Add(copy, opts.activate); err != nil {
		if errors.Is(err, hivectl.ErrProfileExists) {
			return &usageError{message: err.Error()}
		}
		return err
	}
	if err := commit(deps, set); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "✓ Created hive session %q for %s with label %q\n", copy.Name, copy.Hub, copy.Session)
	return nil
}

// ── rename ──────────────────────────────────────────────────────────────────

func newHivesRenameCommand(env *commandEnv) *cobra.Command {
	return &cobra.Command{
		Use:   "rename <old> <new>",
		Short: "Rename a hive profile",
		Args:  argsExact(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return env.runHivesRename(cmd, args[0], args[1])
		},
	}
}

func (e *commandEnv) runHivesRename(cmd *cobra.Command, oldName, newName string) error {
	if err := hivectl.ValidateProfileName(newName); err != nil {
		return &usageError{message: err.Error()}
	}
	deps, err := e.hivesDeps()
	if err != nil {
		return err
	}
	set, err := loadProfiles(cmd, deps, false)
	if err != nil {
		return err
	}
	if existing, _ := set.Find(oldName); existing == nil {
		return unknownHiveError(oldName, set)
	}
	target, err := set.Rename(oldName, newName)
	if err != nil {
		if clash, _ := set.Find(newName); errors.Is(err, hivectl.ErrProfileExists) && clash != nil {
			return &usageError{message: fmt.Sprintf("a hive profile named %q already exists (%s)", clash.Name, clash.Hub)}
		}
		return &usageError{message: err.Error()}
	}
	if err := commit(deps, set); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "✓ Renamed hive %q to %q (%s)\n", oldName, newName, target.Hub)
	return nil
}

// ── remove ──────────────────────────────────────────────────────────────────

func newHivesRemoveCommand(env *commandEnv) *cobra.Command {
	yes := false
	cmd := &cobra.Command{
		Use:     "remove <name>",
		Aliases: []string{"rm"},
		Short:   "Forget a hive profile and its registration token",
		Long: "Removes the profile from profiles.yml and regenerates contributor.env without it.\n\n" +
			"This discards a live registration token that the hub will never reprint: re-adding the hive " +
			"means registering again, or moving the identity with 'just contribute-move'. The previous " +
			"contributor.env is kept at contributor.env.bak. Confirmation is required unless --yes is given.",
		Args: argsExact(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return env.runHivesRemove(cmd, args[0], yes)
		},
	}
	cmd.Flags().BoolVar(&yes, "yes", false, "skip the confirmation prompt")
	return cmd
}

func (e *commandEnv) runHivesRemove(cmd *cobra.Command, name string, yes bool) error {
	deps, err := e.hivesDeps()
	if err != nil {
		return err
	}
	set, err := loadProfiles(cmd, deps, false)
	if err != nil {
		return err
	}
	target, _ := set.Find(name)
	if target == nil {
		return unknownHiveError(name, set)
	}
	resolved := target.Name
	hub := target.Hub
	if !yes {
		// The prompt comes BEFORE the mutation, so declining leaves the set
		// exactly as it was loaded and nothing is written.
		confirmed, err := confirmRemoval(cmd, resolved, hub)
		if err != nil {
			return err
		}
		if !confirmed {
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Left %q in place.\n", resolved)
			return nil
		}
	}
	_, wasActive, err := set.Remove(resolved)
	if err != nil {
		return unknownHiveError(name, set)
	}
	if err := commit(deps, set); err != nil {
		return err
	}
	out := cmd.OutOrStdout()
	_, _ = fmt.Fprintf(out, "✓ Removed hive %q (%s)\n", resolved, hub)
	_, _ = fmt.Fprintf(out, "  previous credentials kept at %s.bak\n", deps.store.EnvPath())
	switch {
	case len(set.Profiles) == 0:
		_, _ = fmt.Fprintln(out, "  no hives left — the relay has nothing to solicit from until you add one")
	case wasActive:
		_, _ = fmt.Fprintf(out, "  active hive is now %q\n", set.Active)
	}
	return nil
}

func confirmRemoval(cmd *cobra.Command, name, hub string) (bool, error) {
	_, _ = fmt.Fprintf(cmd.OutOrStdout(),
		"Remove hive %q (%s)?\nThis discards its registration token, which the hub cannot reprint.\nType the hive name to confirm: ", name, hub)
	reader := bufio.NewReader(cmd.InOrStdin())
	line, err := reader.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return false, fmt.Errorf("read confirmation: %w", err)
	}
	return strings.EqualFold(strings.TrimSpace(line), name), nil
}

// ── shared helpers ──────────────────────────────────────────────────────────

func unknownHiveError(name string, set *hivectl.ProfileSet) error {
	known := make([]string, 0, len(set.Profiles))
	for _, p := range set.Profiles {
		known = append(known, p.Name)
	}
	if len(known) == 0 {
		return fmt.Errorf("%w: %q (no hives are configured)", hivectl.ErrProfileNotFound, name)
	}
	return fmt.Errorf("%w: %q (configured: %s)", hivectl.ErrProfileNotFound, name, strings.Join(known, ", "))
}

// httpRegistrar is the production hubRegistrar: the timeout this command run
// was given, bound to the shared registration call.
type httpRegistrar struct {
	timeout time.Duration
}

func (r httpRegistrar) Register(ctx context.Context, base, githubUser string) (hubRegistration, error) {
	return hivectl.Register(ctx, base, githubUser, r.timeout)
}

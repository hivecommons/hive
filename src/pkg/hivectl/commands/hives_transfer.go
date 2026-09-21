package commands

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/charmbracelet/x/term"
	"github.com/hivecommons/hive/pkg/hivectl"
	"github.com/spf13/cobra"
)

// `hivectl hives export|import|session` — moving one hive profile between
// machines, and running two relays against one hive as two named sessions
// (#8127, phase 3 of #8097).
//
// Before this, "use this identity on the laptop too" meant
// `scp ~/.config/hive/contributor.env`: the whole positional file, every hive
// in it, with every registration token in the clear. export/import narrows that
// to ONE profile and encrypts it under a passphrase the operator types on the
// far end, so the credential is never at rest outside 0600 files on the two
// machines that are meant to hold it.
//
// What export does NOT change is the identity model. A registration token is
// the identity; importing it on a second machine means both machines
// authenticate as the same contributor, and the hub sees two connections. That
// is legitimate and sometimes what you want — it is also why `hives session`
// exists, because two relays sharing one contributor id without distinct
// HIVE_SESSION labels collide on a single active-task slot.

// ── export ──────────────────────────────────────────────────────────────────

func newHivesExportCommand(env *commandEnv) *cobra.Command {
	var (
		out             string
		passphraseStdin bool
		force           bool
	)
	cmd := &cobra.Command{
		Use:   "export <name>",
		Short: "Write one hive profile to a passphrase-encrypted bundle",
		Long: "Seals one profile — hub, contributor id, registration token, session label and backend " +
			"defaults — into a file encrypted with a passphrase you choose. Import it on the other machine " +
			"with 'hivectl hives import'.\n\n" +
			"The bundle is never written in plaintext: it is AES-256-GCM under a PBKDF2-HMAC-SHA256 key, " +
			"so the file is safe to move over scp, a share or a USB stick as long as the passphrase travels " +
			"separately. The passphrase is not recoverable and is not stored anywhere — losing it means " +
			"the bundle is scrap, though the profile on this machine is untouched.\n\n" +
			"Importing the bundle elsewhere does NOT create a second identity: the token IS the identity, " +
			"so both machines then authenticate as the same contributor and the hub sees two connections. " +
			"Run only one relay per identity unless each gives itself a session label ('hivectl hives session').",
		Example: `  hivectl hives export acme
  hivectl hives export acme --out /media/usb/acme.hiveprofile
  printf '%s' "$PASSPHRASE" | hivectl hives export acme --out - --passphrase-stdin > acme.hiveprofile`,
		Args: argsExact(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return env.runHivesExport(cmd, args[0], out, passphraseStdin, force)
		},
	}
	// No shorthand: -o is the root command's --output format flag.
	cmd.Flags().StringVar(&out, "out", "", "file to write (default <name>.hiveprofile; '-' for stdout)")
	cmd.Flags().BoolVar(&passphraseStdin, "passphrase-stdin", false, "read the passphrase from stdin instead of prompting")
	cmd.Flags().BoolVar(&force, "force", false, "overwrite the output file if it already exists")
	return cmd
}

func (e *commandEnv) runHivesExport(cmd *cobra.Command, name, out string, passphraseStdin, force bool) error {
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
	profile := *target

	passphrase, err := readBundlePassphrase(cmd, deps, passphraseStdin, true)
	if err != nil {
		return err
	}
	bundle, err := hivectl.ExportProfile(profile, passphrase)
	if err != nil {
		return err
	}

	if out == "-" {
		_, err = cmd.OutOrStdout().Write(bundle)
		return err
	}
	if strings.TrimSpace(out) == "" {
		out = hivectl.DefaultBundleFileName(profile.Name)
	}
	if err := writeBundleFile(out, bundle, force); err != nil {
		return err
	}

	stdout := cmd.OutOrStdout()
	_, _ = fmt.Fprintf(stdout, "✓ Exported hive %q (%s) to %s\n", profile.Name, profile.Hub, out)
	_, _ = fmt.Fprintln(stdout, "  encrypted with your passphrase; the file holds no plaintext token")
	_, _ = fmt.Fprintf(stdout, "  import it with: hivectl hives import %s\n", out)
	_, _ = fmt.Fprintln(stdout, "  send the passphrase by a different channel from the file, and delete the bundle once imported")
	_, _ = fmt.Fprintln(stdout, "  both machines will then authenticate as the same contributor — run one relay per identity,")
	_, _ = fmt.Fprintln(stdout, "  or give each a session label ('hivectl hives session <name> --label <x>')")
	return nil
}

// writeBundleFile creates the bundle at 0600 and refuses to clobber an existing
// file unless asked. O_EXCL rather than a stat-then-write: the thing being
// overwritten could be somebody else's exported credential, and a check that
// races is not a check.
func writeBundleFile(path string, data []byte, force bool) error {
	flags := os.O_WRONLY | os.O_CREATE | os.O_EXCL
	if force {
		flags = os.O_WRONLY | os.O_CREATE | os.O_TRUNC
	}
	f, err := os.OpenFile(path, flags, 0o600)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return fmt.Errorf("%s already exists; pick another --out or pass --force to overwrite it", path)
		}
		return fmt.Errorf("write bundle %s: %w", path, err)
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return fmt.Errorf("write bundle %s: %w", path, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("write bundle %s: %w", path, err)
	}
	// An existing file opened with --force keeps its old mode, which may be
	// wider than 0600. Pin it, for the same reason profiles.yml is pinned.
	if err := os.Chmod(path, 0o600); err != nil {
		return fmt.Errorf("restrict permissions on %s: %w", path, err)
	}
	return nil
}

// ── import ──────────────────────────────────────────────────────────────────

func newHivesImportCommand(env *commandEnv) *cobra.Command {
	var (
		newName         string
		activate        bool
		passphraseStdin bool
	)
	cmd := &cobra.Command{
		Use:   "import <bundle>",
		Short: "Add a hive profile from a passphrase-encrypted bundle",
		Long: "Prompts for the passphrase, opens a bundle written by 'hivectl hives export', checks the " +
			"profile against the same rules profiles.yml enforces, and appends it.\n\n" +
			"Nothing is written until the bundle has decrypted and validated, so a wrong passphrase leaves " +
			"your profiles exactly as they were. If a profile of that name already exists, pass --name to " +
			"store it under another one.",
		Example: `  hivectl hives import acme.hiveprofile
  hivectl hives import acme.hiveprofile --name acme-laptop --activate
  printf '%s' "$PASSPHRASE" | hivectl hives import acme.hiveprofile --passphrase-stdin`,
		Args: argsExact(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return env.runHivesImport(cmd, args[0], newName, activate, passphraseStdin)
		},
	}
	cmd.Flags().StringVar(&newName, "name", "", "store the imported hive under this name instead of the one in the bundle")
	cmd.Flags().BoolVar(&activate, "activate", false, "make the imported hive the active one")
	cmd.Flags().BoolVar(&passphraseStdin, "passphrase-stdin", false, "read the passphrase from stdin instead of prompting")
	return cmd
}

func (e *commandEnv) runHivesImport(cmd *cobra.Command, path, newName string, activate, passphraseStdin bool) error {
	if newName != "" {
		if err := hivectl.ValidateProfileName(newName); err != nil {
			return &usageError{message: err.Error()}
		}
	}
	if path == "-" && passphraseStdin {
		return &usageError{message: "the bundle and the passphrase cannot both come from stdin; write the bundle to a file, or drop --passphrase-stdin"}
	}

	data, err := readBundleFile(cmd, path)
	if err != nil {
		return err
	}
	deps, err := e.hivesDeps()
	if err != nil {
		return err
	}
	// allowEmpty: importing is exactly the "this machine has nothing yet" case
	// the whole command exists for.
	set, err := loadProfiles(cmd, deps, true)
	if err != nil {
		return err
	}

	passphrase, err := readBundlePassphrase(cmd, deps, passphraseStdin, false)
	if err != nil {
		return err
	}
	profile, err := hivectl.ImportProfile(data, passphrase)
	if err != nil {
		if errors.Is(err, hivectl.ErrBadPassphrase) {
			return fmt.Errorf("%s: %w\n  nothing was written; try again, or re-export from the machine that holds the profile", path, err)
		}
		return fmt.Errorf("%s: %w", path, err)
	}

	if newName != "" {
		profile.Name = newName
	}
	if clash, _ := set.Find(profile.Name); clash != nil {
		return &usageError{message: fmt.Sprintf(
			"a hive profile named %q already exists (%s); import it under another name with --name <name>, or 'hivectl hives remove %s' first",
			clash.Name, clash.Hub, clash.Name)}
	}

	// Same hub AND same contributor id means this machine already holds this
	// identity under another name. Not an error — that is how you build two
	// session-labelled entries for one hive — but worth saying out loud,
	// because two relays on one identity with no session labels contend for a
	// single task slot on the hub.
	var duplicate *hivectl.Profile
	for i := range set.Profiles {
		if strings.EqualFold(set.Profiles[i].Hub, profile.Hub) && set.Profiles[i].ContributorID == profile.ContributorID && profile.ContributorID != "" {
			duplicate = &set.Profiles[i]
			break
		}
	}

	set.Profiles = append(set.Profiles, profile)
	if activate || set.Active == "" {
		set.Active = profile.Name
	}
	if err := commit(deps, set); err != nil {
		return err
	}

	stdout := cmd.OutOrStdout()
	_, _ = fmt.Fprintf(stdout, "✓ Imported hive %q (%s)\n", profile.Name, profile.Hub)
	if profile.ContributorID != "" {
		_, _ = fmt.Fprintf(stdout, "  contributor id: %s\n", profile.ContributorID)
	}
	if label, ok := profile.SessionLabel(); ok {
		_, _ = fmt.Fprintf(stdout, "  session label: %s\n", sessionColumn(&label))
	}
	if duplicate != nil {
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(),
			"Note: %q already points at %s as contributor %s. Both entries are the SAME identity, so give them different session labels ('hivectl hives session') before running two relays, or the hub keys both to one task slot.\n",
			duplicate.Name, duplicate.Hub, duplicate.ContributorID)
	}
	if strings.EqualFold(set.Active, profile.Name) {
		_, _ = fmt.Fprintln(stdout, "  active: yes — the relay will solicit from this hive first")
	}
	_, _ = fmt.Fprintf(stdout, "  saved to %s; %s regenerated\n", deps.store.Path(), deps.store.EnvPath())
	_, _ = fmt.Fprintf(stdout, "  delete %s now that it is imported — it still decrypts to a live token\n", path)
	_, _ = fmt.Fprintln(stdout, "  'just contribute-hive' will register as this contributor id without copying any file by hand")
	return nil
}

func readBundleFile(cmd *cobra.Command, path string) ([]byte, error) {
	if path == "-" {
		data, err := io.ReadAll(io.LimitReader(cmd.InOrStdin(), 1<<20))
		if err != nil {
			return nil, fmt.Errorf("read bundle from stdin: %w", err)
		}
		return data, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read bundle %s: %w", path, err)
	}
	return data, nil
}

// ── session ─────────────────────────────────────────────────────────────────

func newHivesSessionCommand(env *commandEnv) *cobra.Command {
	var (
		label    string
		as       string
		activate bool
	)
	cmd := &cobra.Command{
		Use:   "session <name> --label <label>",
		Short: "Copy a hive profile under a session label, so one hive runs as two sessions",
		Long: "One GitHub account has one contributor identity per hive, and the hub keys task leases, " +
			"cooldowns and ownership on that identity — so two relays under one account collide on a single " +
			"active-task slot unless each declares a session label (HIVE_SESSION).\n\n" +
			"This copies an existing profile under a new name with that label set, so \"two sessions against " +
			"hive X\" is simply two entries. The copy shares the original's hub, contributor id and " +
			"registration token: it is the same identity, scoped to a different session on the hub " +
			"(contributor_id#label).\n\n" +
			"The label is projected into contributor.env for whichever entry is ACTIVE, so two simultaneous " +
			"relays want two config directories (HOME-scoped) or two containers, one per entry.",
		Example: `  hivectl hives session acme --label review
  hivectl hives session acme --label nightly --as acme-nightly --activate`,
		Args: argsExact(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if !cmd.Flags().Changed("label") {
				return &usageError{message: "--label is required: the HIVE_SESSION label the copy runs under, e.g. --label review"}
			}
			return env.runHivesSession(cmd, args[0], label, as, activate)
		},
	}
	cmd.Flags().StringVar(&label, "label", "", "session label for the new profile (required)")
	cmd.Flags().StringVar(&as, "as", "", "name for the new profile (default <name>-<label>)")
	cmd.Flags().BoolVar(&activate, "activate", false, "make the new profile the active one")
	return cmd
}

func (e *commandEnv) runHivesSession(cmd *cobra.Command, name, label, as string, activate bool) error {
	label = strings.TrimSpace(label)
	deps, err := e.hivesDeps()
	if err != nil {
		return err
	}
	set, err := loadProfiles(cmd, deps, false)
	if err != nil {
		return err
	}
	source, _ := set.Find(name)
	if source == nil {
		return unknownHiveError(name, set)
	}

	newName := strings.TrimSpace(as)
	if newName == "" {
		// The default name is derived, not generated: `acme` + `review` reads
		// as one thing in `hives list` and in the container name the relay
		// launch path builds. An empty label (the opt-out) has nothing to
		// derive from, so that case must name itself with --as.
		if label == "" {
			return &usageError{message: "--label was empty, so there is no name to derive; pass --as <name> to say what the unlabelled copy should be called"}
		}
		newName = source.Name + "-" + label
	}
	if err := hivectl.ValidateProfileName(newName); err != nil {
		return &usageError{message: fmt.Sprintf("%v (pass --as <name> to choose one)", err)}
	}
	if clash, _ := set.Find(newName); clash != nil {
		return &usageError{message: fmt.Sprintf("a hive profile named %q already exists (%s); pass --as <name> to pick another", clash.Name, clash.Hub)}
	}

	copied := source.WithSession(label)
	copied.Name = newName
	copied.AddedAt = deps.now().UTC().Truncate(time.Second)

	set.Profiles = append(set.Profiles, copied)
	if activate {
		set.Active = copied.Name
	}
	if err := commit(deps, set); err != nil {
		return err
	}

	stdout := cmd.OutOrStdout()
	_, _ = fmt.Fprintf(stdout, "✓ Added hive %q — %s as session %s\n", copied.Name, copied.Hub, sessionColumn(&label))
	_, _ = fmt.Fprintf(stdout, "  same contributor identity as %q; the hub scopes it as a separate session\n", source.Name)
	if activate {
		_, _ = fmt.Fprintf(stdout, "  active: yes — contributor.env now carries HIVE_SESSION=%s\n", label)
	} else {
		_, _ = fmt.Fprintf(stdout, "  %q is still active; 'hivectl hives use %s' projects the new label\n", set.Active, copied.Name)
	}
	_, _ = fmt.Fprintln(stdout, "  to run BOTH at once, give each relay its own config dir (HOME) or container — one active entry projects one label")
	return nil
}

// ── passphrase input ────────────────────────────────────────────────────────

// readBundlePassphrase gets the passphrase either from stdin (scripted) or from
// the terminal with echo off (interactive).
//
// There is deliberately no --passphrase flag and no environment variable: a
// flag value lands in the shell history and in every `ps` on the machine, and
// an environment variable is inherited by everything the process spawns. The
// same reasoning is why hivectl reads dashboard tokens from an env var name
// rather than a flag — here even that is too wide, because this secret protects
// a credential the hub cannot reissue.
func readBundlePassphrase(cmd *cobra.Command, deps *hivesDeps, fromStdin, confirm bool) (string, error) {
	if fromStdin {
		data, err := io.ReadAll(io.LimitReader(cmd.InOrStdin(), 4096))
		if err != nil {
			return "", fmt.Errorf("read passphrase from stdin: %w", err)
		}
		// Only the trailing newline goes: a passphrase may legitimately start
		// or end with a space, and silently trimming it would make a bundle
		// that cannot be opened by the passphrase its owner believes they set.
		passphrase := strings.TrimRight(string(data), "\r\n")
		if passphrase == "" {
			return "", &usageError{message: "--passphrase-stdin was given but stdin was empty"}
		}
		return passphrase, nil
	}
	return deps.passphrase(cmd, confirm)
}

// The terminal calls are behind vars for the same reason runGH is: a test
// process has no controlling terminal, so the only way to exercise the
// confirm/mismatch branches of a prompt is to substitute the two calls that
// need one. Production leaves them alone.
var (
	isTerminal         = term.IsTerminal
	readPasswordNoEcho = term.ReadPassword
)

// promptPassphrase is the production reader: it asks on the terminal with echo
// off, and asks twice when the answer is about to become the only thing
// standing between a bundle and whoever picks the file up.
func promptPassphrase(cmd *cobra.Command, confirm bool) (string, error) {
	file, ok := cmd.InOrStdin().(*os.File)
	if !ok || !isTerminal(file.Fd()) {
		return "", &usageError{message: "no terminal to prompt on; pipe the passphrase in with --passphrase-stdin"}
	}
	// The prompt goes to stderr so `hives export --out -` can be redirected
	// into a file without the prompt landing in the bundle.
	errOut := cmd.ErrOrStderr()
	_, _ = fmt.Fprint(errOut, "Passphrase: ")
	first, err := readPasswordNoEcho(file.Fd())
	// The newline the operator's Return did not echo, so whatever prints next
	// does not land on the prompt line.
	_, _ = fmt.Fprintln(errOut)
	if err != nil {
		return "", fmt.Errorf("read passphrase: %w", err)
	}
	if !confirm {
		return string(first), nil
	}
	_, _ = fmt.Fprint(errOut, "Confirm passphrase: ")
	second, err := readPasswordNoEcho(file.Fd())
	_, _ = fmt.Fprintln(errOut)
	if err != nil {
		return "", fmt.Errorf("read passphrase confirmation: %w", err)
	}
	if string(first) != string(second) {
		return "", errors.New("the two passphrases did not match; nothing was written")
	}
	return string(first), nil
}

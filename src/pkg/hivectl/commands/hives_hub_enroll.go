package commands

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/charmbracelet/x/term"
	"github.com/hivecommons/hive/pkg/hivectl"
	"github.com/spf13/cobra"
)

// hubAdoptionRow is deliberately free of Profile: registration tokens never
// cross the command output boundary. A row is also the review shown before
// confirmation, so the operator can see every endpoint that would be touched.
type hubAdoptionRow struct {
	Hub         string `json:"hub" yaml:"hub"`
	ID          string `json:"id" yaml:"id"`
	Endpoint    string `json:"endpoint" yaml:"endpoint"`
	ProjectName string `json:"project_name,omitempty" yaml:"project_name,omitempty"`
	Org         string `json:"org,omitempty" yaml:"org,omitempty"`
	Profile     string `json:"profile,omitempty" yaml:"profile,omitempty"`
	Status      string `json:"status" yaml:"status"`
	Reused      bool   `json:"reused,omitempty" yaml:"reused,omitempty"`
	Removed     bool   `json:"removed,omitempty" yaml:"removed,omitempty"`
}

type hivesHubAdoptOptions struct {
	hives  []string
	follow string
	yes    bool
}

func newHivesHubAdoptCommand(env *commandEnv) *cobra.Command {
	opts := &hivesHubAdoptOptions{}
	cmd := &cobra.Command{
		Use:     "adopt <name>",
		Example: "  hivectl hives hub adopt commons --follow current",
		Short:   "Review and enroll a tracked hub's advertised Hives",
		Long: "Discovers the tracked hub's Hives, shows which profiles would be " +
			"reused or registered, and asks once before any registration. Existing " +
			"profiles are never re-registered or rotated; --follow selects the " +
			"repository policy for newly enrolled Hives.",
		Args: argsExact(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return env.runHivesHubAdopt(cmd, args[0], opts)
		},
	}
	cmd.Flags().StringArrayVar(&opts.hives, "hive", nil, "only enroll this advertised Hive URL (repeat for multiple Hives)")
	cmd.Flags().StringVar(&opts.follow, "follow", "", "policy for newly enrolled Hives: off, current, or current-and-future-repos (default: tracked hub policy)")
	cmd.Flags().BoolVar(&opts.yes, "yes", false, "enroll the reviewed Hives without an interactive prompt")
	return cmd
}

func newHivesHubFollowCommand(env *commandEnv) *cobra.Command {
	var mode string
	cmd := &cobra.Command{
		Use:     "follow <name> --mode <policy>",
		Example: "  hivectl hives hub follow commons --mode current-and-future-repos",
		Short:   "Change a tracked hub's future-enrollment policy",
		Long: "Changes only the tracked hub policy. It does not enroll a Hive, " +
			"register a credential, or change any current repository membership.",
		Args: argsExact(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if !cmd.Flags().Changed("mode") || strings.TrimSpace(mode) == "" {
				return &usageError{message: "--mode is required: choose off, current, or current-and-future-repos"}
			}
			mode = strings.TrimSpace(mode)
			if err := hivectl.ValidateHubFollow(mode); err != nil {
				return &usageError{message: err.Error()}
			}
			deps, err := env.hivesDeps()
			if err != nil {
				return err
			}
			set, err := loadProfiles(cmd, deps, true)
			if err != nil {
				return err
			}
			hub, _ := set.FindHub(args[0])
			if hub == nil {
				return unknownHubError(args[0], set)
			}
			if hub.EffectiveFollow() == mode {
				_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Hub %q already follows %s; current membership is unchanged.\n", hub.Name, mode)
				return nil
			}
			hub.Follow = mode
			if err := commit(deps, set); err != nil {
				return err
			}
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Hub %q now follows %s. Current Hive membership is unchanged.\n", hub.Name, mode)
			return nil
		},
	}
	cmd.Flags().StringVar(&mode, "mode", "", "follow policy: off, current, or current-and-future-repos")
	return cmd
}

func newHivesHubRefreshCommand(env *commandEnv) *cobra.Command {
	var yes bool
	cmd := &cobra.Command{
		Use:     "refresh [name]",
		Example: "  hivectl hives hub refresh commons --yes",
		Short:   "Rediscover tracked Hives and enroll newly advertised ones",
		Long: "Re-fetches one tracked hub or every tracked hub, reports added and " +
			"removed advertisements, and applies the saved follow policy. A hub " +
			"with follow=off still needs an explicit confirmation before new Hives " +
			"are enrolled.",
		Args: argsMax(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return env.runHivesHubRefresh(cmd, optionalHubName(args), yes)
		},
	}
	cmd.Flags().BoolVar(&yes, "yes", false, "enroll newly advertised Hives without an interactive prompt")
	return cmd
}

func optionalHubName(args []string) string {
	if len(args) == 0 {
		return ""
	}
	return args[0]
}

func (e *commandEnv) runHivesHubAdopt(cmd *cobra.Command, name string, opts *hivesHubAdoptOptions) error {
	if !validOutput(e.options.output) {
		return &usageError{message: fmt.Sprintf("invalid --output %q; expected table, json, yaml, or jsonl", e.options.output)}
	}
	if opts.follow != "" {
		if err := hivectl.ValidateHubFollow(opts.follow); err != nil {
			return &usageError{message: err.Error()}
		}
	}
	deps, err := e.hivesDeps()
	if err != nil {
		return err
	}
	set, err := loadProfiles(cmd, deps, true)
	if err != nil {
		return err
	}
	hub, _ := set.FindHub(name)
	if hub == nil {
		return unknownHubError(name, set)
	}
	hives, err := hivectl.DiscoverHubHives(cmd.Context(), hubDiscoveryClient(e), hub.URL)
	if err != nil {
		return fmt.Errorf("cannot adopt Hives from hub %q: %w", hub.Name, err)
	}
	hives, err = selectHubHives(hives, opts.hives)
	if err != nil {
		return &usageError{message: err.Error()}
	}
	follow := hub.EffectiveFollow()
	if opts.follow != "" {
		follow = opts.follow
	}
	candidates, rows, candidateErrors := planHubAdoption(set, hub.Name, hives)
	if e.options.output == "table" {
		printHubAdoptionTable(cmd.OutOrStdout(), fmt.Sprintf("Review hub %q (%s), follow=%s", hub.Name, hub.URL, follow), rows)
	}
	if len(candidateErrors) > 0 && len(candidates) == 0 {
		return joinHubErrors("hub adoption", candidateErrors)
	}
	if len(candidates) > 0 {
		confirmed, err := confirmHubEnrollment(cmd, opts.yes, fmt.Sprintf("Enroll %d new Hive(s) from hub %q?", len(candidates), hub.Name))
		if err != nil {
			return err
		}
		if !confirmed {
			_, _ = fmt.Fprintln(cmd.OutOrStdout(), "No Hives enrolled; profiles.yml was left unchanged.")
			return nil
		}
	}

	changed := false
	if opts.follow != "" && hub.EffectiveFollow() != follow {
		hub.Follow = follow
		changed = true
	}
	reviewed := hubHiveIDs(hives)
	failedReview := map[string]bool{}
	for i := range candidates {
		candidate := &candidates[i]
		row := &rows[candidate.row]
		var repositories []string
		if follow != hivectl.HubFollowOff {
			repositories, err = discoverSelectionRepositories(cmd, candidate.hive.HubURL, e)
			if err != nil {
				row.Status = "unavailable: repository discovery failed: " + err.Error()
				failedReview[candidate.hive.ID] = true
				candidateErrors = append(candidateErrors, fmt.Sprintf("%s: %s", candidate.hive.ID, row.Status))
				continue
			}
		}
		profileOpts := &hivesAddOptions{hub: candidate.hive.HubURL}
		profile, registerErr := e.registerHive(cmd, deps, candidate.name, profileOpts)
		if registerErr != nil {
			row.Status = "registration failed: " + registerErr.Error()
			failedReview[candidate.hive.ID] = true
			candidateErrors = append(candidateErrors, fmt.Sprintf("%s: %s", candidate.hive.ID, row.Status))
			continue
		}
		if err := set.Add(profile, false); err != nil {
			row.Status = "registration failed: save profile: " + err.Error()
			failedReview[candidate.hive.ID] = true
			candidateErrors = append(candidateErrors, fmt.Sprintf("%s: %s", candidate.hive.ID, row.Status))
			continue
		}
		if err := applyHubFollowSelection(set, profile.Name, follow, repositories); err != nil {
			row.Status = "ready (repository policy unavailable: " + err.Error() + ")"
			candidateErrors = append(candidateErrors, fmt.Sprintf("%s: %s", candidate.hive.ID, err.Error()))
		} else {
			row.Status = "ready"
		}
		row.Profile = profile.Name
		changed = true
	}
	if len(opts.hives) > 0 {
		// A narrowed --hive adoption reviews only the selected Hives; keep the
		// IDs reviewed by earlier runs so a later refresh does not treat them
		// (including ones deliberately removed with `hives remove`) as new.
		reviewed = mergeReviewedIDs(hub.Reviewed, reviewed)
	}
	reviewed = filterReviewedIDs(reviewed, failedReview)
	if !sameStrings(hub.Reviewed, reviewed) {
		hub.Reviewed = reviewed
		changed = true
	}
	if len(candidateErrors) > 0 && !changed {
		if e.options.output != "table" {
			if printErr := e.print(cmd, rows); printErr != nil {
				return printErr
			}
		}
		return joinHubErrors("hub adoption", candidateErrors)
	}
	if changed {
		if err := commit(deps, set); err != nil {
			return err
		}
	}
	if e.options.output != "table" {
		if err := e.print(cmd, rows); err != nil {
			return err
		}
	} else {
		printHubAdoptionTable(cmd.OutOrStdout(), "Hub adoption result", rows)
	}
	if changed {
		if err := reloadHivesAfterEdit(cmd, deps); err != nil {
			return err
		}
	}
	if len(candidateErrors) > 0 {
		return joinHubErrors("hub adoption completed with failures", candidateErrors)
	}
	return nil
}

func (e *commandEnv) runHivesHubRefresh(cmd *cobra.Command, name string, yes bool) error {
	if !validOutput(e.options.output) {
		return &usageError{message: fmt.Sprintf("invalid --output %q; expected table, json, yaml, or jsonl", e.options.output)}
	}
	deps, err := e.hivesDeps()
	if err != nil {
		return err
	}
	set, err := loadProfiles(cmd, deps, true)
	if err != nil {
		return err
	}
	hubs, err := hubsToRefresh(set, name)
	if err != nil {
		return err
	}
	plans := make([]hubRefreshPlan, 0, len(hubs))
	var failures []string
	for _, hub := range hubs {
		plan := hubRefreshPlan{hub: hub}
		hives, discoverErr := hivectl.DiscoverHubHives(cmd.Context(), hubDiscoveryClient(e), hub.URL)
		if discoverErr != nil {
			plan.rows = append(plan.rows, hubAdoptionRow{Hub: hub.Name, Status: "unavailable: " + discoverErr.Error()})
			plan.err = discoverErr
			failures = append(failures, fmt.Sprintf("%s: %v", hub.Name, discoverErr))
			plans = append(plans, plan)
			continue
		}
		plan.hives = hives
		plan.reviewed = hubHiveIDs(hives)
		previous := hubReviewedSet(hub.Reviewed)
		for _, hive := range hives {
			row := hubAdoptionRow{Hub: hub.Name, ID: hive.ID, Endpoint: hive.HubURL, ProjectName: hive.ProjectName, Org: hive.Org}
			profile, profileErr := set.FindHive(hive.HubURL)
			isNew := !previous[hive.ID]
			if profileErr != nil {
				row.Status = "unavailable: " + profileErr.Error()
				failures = append(failures, fmt.Sprintf("%s/%s: %s", hub.Name, hive.ID, row.Status))
			} else if profile != nil {
				row.Profile, row.Reused, row.Status = profile.Name, true, "ready"
			} else if isNew {
				row.Status = "to enroll"
				plan.targets = append(plan.targets, hubEnrollmentTarget{hive: hive, row: len(plan.rows)})
			} else {
				row.Status = "not onboarded"
			}
			plan.rows = append(plan.rows, row)
		}
		current := map[string]struct{}{}
		for _, hive := range hives {
			current[hive.ID] = struct{}{}
		}
		for _, id := range sortedReviewedIDs(hub.Reviewed) {
			if _, present := current[id]; !present {
				plan.rows = append(plan.rows, hubAdoptionRow{Hub: hub.Name, ID: id, Status: "no longer advertised", Removed: true})
			}
		}
		plans = append(plans, plan)
	}

	var pendingConfirmation int
	for _, plan := range plans {
		if plan.err != nil || plan.hub.EffectiveFollow() != hivectl.HubFollowOff {
			continue
		}
		for _, target := range plan.targets {
			if plan.rows[target.row].Status == "to enroll" {
				pendingConfirmation++
			}
		}
	}
	if pendingConfirmation > 0 {
		if e.options.output == "table" {
			printHubRefreshTable(cmd.OutOrStdout(), "Review hub refresh", plans)
		}
		confirmed, confirmErr := confirmHubEnrollment(cmd, yes, fmt.Sprintf("Enroll %d newly advertised Hive(s)?", pendingConfirmation))
		if confirmErr != nil {
			return confirmErr
		}
		if !confirmed {
			_, _ = fmt.Fprintln(cmd.OutOrStdout(), "No Hives enrolled; profiles.yml was left unchanged.")
			return nil
		}
	}

	changed := false
	for planIndex := range plans {
		plan := &plans[planIndex]
		if plan.err != nil {
			continue
		}
		failedReview := map[string]bool{}
		for _, row := range plan.rows {
			if !row.Removed && strings.HasPrefix(row.Status, "unavailable:") {
				failedReview[row.ID] = true
			}
		}
		for _, target := range plan.targets {
			row := &plan.rows[target.row]
			existing, profileErr := set.FindHive(target.hive.HubURL)
			if profileErr != nil {
				row.Status = "unavailable: " + profileErr.Error()
				failedReview[target.hive.ID] = true
				failures = append(failures, fmt.Sprintf("%s/%s: %s", plan.hub.Name, target.hive.ID, row.Status))
				continue
			}
			if existing != nil {
				row.Profile, row.Reused, row.Status = existing.Name, true, "ready"
				continue
			}
			follow := plan.hub.EffectiveFollow()
			var repositories []string
			if follow != hivectl.HubFollowOff {
				repositories, err = discoverSelectionRepositories(cmd, target.hive.HubURL, e)
				if err != nil {
					row.Status = "unavailable: repository discovery failed: " + err.Error()
					failedReview[target.hive.ID] = true
					failures = append(failures, fmt.Sprintf("%s/%s: %s", plan.hub.Name, target.hive.ID, row.Status))
					continue
				}
			}
			profileName := nextHubProfileName(set, target.hive.HubURL)
			profile, registerErr := e.registerHive(cmd, deps, profileName, &hivesAddOptions{hub: target.hive.HubURL})
			if registerErr != nil {
				row.Status = "registration failed: " + registerErr.Error()
				failedReview[target.hive.ID] = true
				failures = append(failures, fmt.Sprintf("%s/%s: %s", plan.hub.Name, target.hive.ID, row.Status))
				continue
			}
			if err := set.Add(profile, false); err != nil {
				row.Status = "registration failed: save profile: " + err.Error()
				failedReview[target.hive.ID] = true
				failures = append(failures, fmt.Sprintf("%s/%s: %s", plan.hub.Name, target.hive.ID, row.Status))
				continue
			}
			if err := applyHubFollowSelection(set, profile.Name, follow, repositories); err != nil {
				row.Status = "ready (repository policy unavailable: " + err.Error() + ")"
				failures = append(failures, fmt.Sprintf("%s/%s: %s", plan.hub.Name, target.hive.ID, err.Error()))
			} else {
				row.Status = "ready"
			}
			row.Profile = profile.Name
			changed = true
		}
		reviewed := filterReviewedIDs(plan.reviewed, failedReview)
		if !sameStrings(plan.hub.Reviewed, reviewed) {
			plan.hub.Reviewed = reviewed
			changed = true
		}
	}
	if changed {
		if err := commit(deps, set); err != nil {
			return err
		}
	}
	if e.options.output == "table" {
		printHubRefreshTable(cmd.OutOrStdout(), "Hub refresh result", plans)
	} else {
		rows := make([]hubAdoptionRow, 0)
		for _, plan := range plans {
			rows = append(rows, plan.rows...)
		}
		if err := e.print(cmd, rows); err != nil {
			return err
		}
	}
	if changed {
		if err := reloadHivesAfterEdit(cmd, deps); err != nil {
			return err
		}
	}
	if len(failures) > 0 {
		return joinHubErrors("hub refresh completed with failures", failures)
	}
	return nil
}

type hubAdoptionCandidate struct {
	hive hivectl.HubHive
	name string
	row  int
}

type hubEnrollmentTarget struct {
	hive hivectl.HubHive
	row  int
}

type hubRefreshPlan struct {
	hub      *hivectl.HubEntry
	hives    []hivectl.HubHive
	reviewed []string
	rows     []hubAdoptionRow
	targets  []hubEnrollmentTarget
	err      error
}

func planHubAdoption(set *hivectl.ProfileSet, hubName string, hives []hivectl.HubHive) ([]hubAdoptionCandidate, []hubAdoptionRow, []string) {
	taken := profileNameSet(set)
	candidates := make([]hubAdoptionCandidate, 0, len(hives))
	rows := make([]hubAdoptionRow, 0, len(hives))
	var failures []string
	for _, hive := range hives {
		row := hubAdoptionRow{Hub: hubName, ID: hive.ID, Endpoint: hive.HubURL, ProjectName: hive.ProjectName, Org: hive.Org}
		profile, err := set.FindHive(hive.HubURL)
		switch {
		case err != nil:
			row.Status = "unavailable: " + err.Error()
			failures = append(failures, fmt.Sprintf("%s: %s", hive.ID, row.Status))
		case profile != nil:
			row.Profile, row.Reused, row.Status = profile.Name, true, "ready"
		default:
			row.Status = "to enroll"
			candidates = append(candidates, hubAdoptionCandidate{hive: hive, name: uniqueHubProfileName(hivectl.ProfileNameFromHub(hive.HubURL), taken), row: len(rows)})
		}
		rows = append(rows, row)
	}
	return candidates, rows, failures
}

func selectHubHives(hives []hivectl.HubHive, requested []string) ([]hivectl.HubHive, error) {
	if len(requested) == 0 {
		return hives, nil
	}
	wanted := make(map[string]string, len(requested))
	for _, raw := range requested {
		id, err := hivectl.NormalizeHiveID(raw)
		if err != nil {
			return nil, fmt.Errorf("--hive %q: %w", raw, err)
		}
		wanted[id] = raw
	}
	selected := make([]hivectl.HubHive, 0, len(wanted))
	found := make(map[string]struct{}, len(wanted))
	for _, hive := range hives {
		if _, ok := wanted[hive.ID]; ok {
			selected = append(selected, hive)
			found[hive.ID] = struct{}{}
		}
	}
	for id, raw := range wanted {
		if _, ok := found[id]; !ok {
			return nil, fmt.Errorf("Hive %q is not advertised by the tracked hub", raw)
		}
	}
	return selected, nil
}

func hubsToRefresh(set *hivectl.ProfileSet, name string) ([]*hivectl.HubEntry, error) {
	if name != "" {
		hub, _ := set.FindHub(name)
		if hub == nil {
			return nil, unknownHubError(name, set)
		}
		return []*hivectl.HubEntry{hub}, nil
	}
	if len(set.Hubs) == 0 {
		return nil, errors.New("no hubs tracked; run 'hivectl hives hub add <name> --url <hub-url>' first")
	}
	hubs := make([]*hivectl.HubEntry, 0, len(set.Hubs))
	for i := range set.Hubs {
		hubs = append(hubs, &set.Hubs[i])
	}
	return hubs, nil
}

func applyHubFollowSelection(set *hivectl.ProfileSet, name, follow string, repositories []string) error {
	if follow == hivectl.HubFollowOff {
		return nil
	}
	if err := set.SetSelectionMode(name, hivectl.SelectionAllCurrent, repositories); err != nil {
		return err
	}
	if follow == hivectl.HubFollowCurrentAndFutureRepos {
		if _, err := set.SetFollowNewRepositories(name, true); err != nil {
			return err
		}
	}
	return nil
}

func confirmHubEnrollment(cmd *cobra.Command, yes bool, question string) (bool, error) {
	if yes {
		return true, nil
	}
	input, ok := cmd.InOrStdin().(*os.File)
	if !ok || !term.IsTerminal(input.Fd()) {
		return false, &usageError{message: "refusing hub enrollment without an interactive terminal; review the Hives and pass --yes to confirm"}
	}
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "%s [y/N]: ", question)
	line, err := bufio.NewReader(cmd.InOrStdin()).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return false, fmt.Errorf("read hub enrollment confirmation: %w", err)
	}
	answer := strings.ToLower(strings.TrimSpace(line))
	return answer == "y" || answer == "yes", nil
}

func printHubAdoptionTable(out io.Writer, heading string, rows []hubAdoptionRow) {
	_, _ = fmt.Fprintln(out, heading)
	_, _ = fmt.Fprintln(out, "ID\tENDPOINT\tPROFILE\tSTATUS")
	for _, row := range rows {
		status := row.Status
		if row.Reused && status == "ready" {
			status = "ready (reused)"
		}
		if row.Removed {
			status = "no longer advertised"
		}
		_, _ = fmt.Fprintf(out, "%s\t%s\t%s\t%s\n", row.ID, row.Endpoint, dashIfEmpty(row.Profile), status)
	}
}

func printHubRefreshTable(out io.Writer, heading string, plans []hubRefreshPlan) {
	_, _ = fmt.Fprintln(out, heading)
	for _, plan := range plans {
		_, _ = fmt.Fprintf(out, "Hub %q (%s), follow=%s\n", plan.hub.Name, plan.hub.URL, plan.hub.EffectiveFollow())
		for _, row := range plan.rows {
			status := row.Status
			if row.Reused && status == "ready" {
				status = "ready (reused)"
			}
			_, _ = fmt.Fprintf(out, "%s\t%s\t%s\t%s\n", row.ID, row.Endpoint, dashIfEmpty(row.Profile), status)
		}
	}
}

func profileNameSet(set *hivectl.ProfileSet) map[string]struct{} {
	taken := make(map[string]struct{}, len(set.Profiles))
	for _, profile := range set.Profiles {
		taken[strings.ToLower(profile.Name)] = struct{}{}
	}
	return taken
}

func uniqueHubProfileName(base string, taken map[string]struct{}) string {
	if base == "" {
		base = "hive"
	}
	candidate := base
	for suffix := 2; ; suffix++ {
		key := strings.ToLower(candidate)
		if _, exists := taken[key]; !exists {
			taken[key] = struct{}{}
			return candidate
		}
		candidate = fmt.Sprintf("%s-%d", base, suffix)
	}
}

func nextHubProfileName(set *hivectl.ProfileSet, hub string) string {
	return uniqueHubProfileName(hivectl.ProfileNameFromHub(hub), profileNameSet(set))
}

func hubHiveIDs(hives []hivectl.HubHive) []string {
	ids := make([]string, 0, len(hives))
	for _, hive := range hives {
		ids = append(ids, hive.ID)
	}
	sort.Strings(ids)
	return ids
}

func sortedReviewedIDs(ids []string) []string {
	out := append([]string(nil), ids...)
	sort.Strings(out)
	return out
}

// mergeReviewedIDs returns the sorted, de-duplicated union of two reviewed ID
// lists. Entries that fail to normalise are kept verbatim so a hand-edited
// profiles.yml never loses data here; load-time validation reports them.
func mergeReviewedIDs(previous, current []string) []string {
	seen := make(map[string]struct{}, len(previous)+len(current))
	merged := make([]string, 0, len(previous)+len(current))
	for _, id := range append(append([]string(nil), previous...), current...) {
		if normalized, err := hivectl.NormalizeHiveID(id); err == nil {
			id = normalized
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		merged = append(merged, id)
	}
	sort.Strings(merged)
	return merged
}

func hubReviewedSet(ids []string) map[string]bool {
	out := make(map[string]bool, len(ids))
	for _, id := range ids {
		if normalized, err := hivectl.NormalizeHiveID(id); err == nil {
			out[normalized] = true
		}
	}
	return out
}

func filterReviewedIDs(ids []string, failed map[string]bool) []string {
	if len(failed) == 0 {
		return ids
	}
	filtered := make([]string, 0, len(ids))
	for _, id := range ids {
		if !failed[id] {
			filtered = append(filtered, id)
		}
	}
	return filtered
}

func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func joinHubErrors(prefix string, failures []string) error {
	if len(failures) == 0 {
		return nil
	}
	return fmt.Errorf("%s: %s", prefix, strings.Join(failures, "; "))
}

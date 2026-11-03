package commands

import (
	"bufio"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/hivectl"
	"github.com/spf13/cobra"
)

// runPresetOnboard stages profiles in memory so the normal preset diff, including
// personal exclusions, is exactly what is confirmed before any registration.
func (e *commandEnv) runPresetOnboard(cmd *cobra.Command, source, only, githubUser string, follow bool) error {
	deps, err := e.hivesDeps()
	if err != nil {
		return err
	}
	set, err := deps.store.LoadForReview()
	if err != nil {
		return err
	}
	if set == nil {
		set = &hivectl.ProfileSet{Version: hivectl.ProfilesVersion}
	}
	if only != "" {
		if p, _ := set.Find(only); p == nil {
			return unknownHiveError(only, set)
		}
	}
	source = presetSource(source)
	preset, err := hivectl.LoadPreset(cmd.Context(), source)
	if err != nil {
		return err
	}
	id := hivectl.PresetReviewID(preset.Name)
	revision, err := hivectl.PresetRevision(preset)
	if err != nil {
		return err
	}
	expanded, hubReports := e.expandPresetOnboard(cmd, preset, id)
	originalCount := len(set.Profiles)
	newProfiles := map[string]bool{}
	// Explicit direct recommendations take precedence over a hub's whole-Hive
	// recommendation. Expansion de-duplicates by origin before staging profiles.
	for _, hive := range expanded.Hives {
		p, err := set.FindHive(hive.URL)
		if err != nil || p != nil || only != "" {
			continue
		}
		name := presetProfileName(set, hive.URL)
		origin, _ := hivectl.NormalizeHiveID(hive.URL)
		relayURL := strings.Replace(strings.Replace(origin, "https://", "wss://", 1), "http://", "ws://", 1) + "/contribute"
		profile := hivectl.Profile{Name: name, Hub: relayURL, AddedAt: deps.now().UTC().Truncate(time.Second),
			Selection: &hivectl.RepositorySelection{Mode: hivectl.SelectionSelected}}
		if active := set.ActiveProfile(); active != nil {
			profile.Backend, profile.Model, profile.Session = active.Backend, active.Model, active.Session
		}
		if err := set.Add(profile, false); err != nil {
			return err
		}
		newProfiles[name] = true
	}
	reports := e.reviewPreset(cmd, set, expanded, id, only, false)
	ready := 0
	for i := range reports {
		r := &reports[i]
		r.PendingRemovals, r.Removals = r.Removals, nil
		if r.Status != "" {
			continue
		}
		p, _ := set.Find(r.Profile)
		// Even an explicit repository list needs a scope-capable discovery
		// endpoint before a new registration can participate. Never fall back
		// to unrestricted membership on an older/offline Hive.
		if newProfiles[r.Profile] && !r.allCurrent {
			if _, err := discoverSelectionRepositories(cmd, p.Hub, e); err != nil {
				r.Status = presetStatusUnavailable
				_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "%s: %v\n", r.Hive, err)
				continue
			}
		}
		ready++
	}
	reports = append(reports, hubReports...)
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Preset source: %s\nRevision: %s\nEnrollment destinations (new profiles require registration):\n", source, revision)
	for _, r := range reports {
		if newProfiles[r.Profile] {
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "  REGISTER %s as %s\n", r.Hive, r.Profile)
		}
	}
	if err := e.printPresetReview(cmd, reports, ""); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Follow future repositories: %t (when set, applies only to reviewed Hives); future hub members: off.\nExisting backend/provider authentication and quota settings are unchanged. No relay will be started.\n", follow)
	if ready == 0 {
		return errors.New("no Hive is ready for onboarding; nothing saved; resolve discovery failures and retry")
	}
	confirmed, err := confirmPreset(cmd, bufio.NewReader(cmd.InOrStdin()), "Enroll new Hives and adopt the repository selections above?")
	if err != nil {
		return err
	}
	if !confirmed {
		_, _ = fmt.Fprintln(cmd.OutOrStdout(), "Not confirmed; no registration or saved selection changes. Run again with interactive confirmation to onboard.")
		return nil
	}
	needsRegistration := false
	for _, r := range reports {
		needsRegistration = needsRegistration || (r.Status == "" && newProfiles[r.Profile])
	}
	if needsRegistration && strings.TrimSpace(githubUser) == "" {
		githubUser, err = deps.githubUser(cmd.Context())
		if err != nil {
			return err
		}
	}
	githubUser = strings.TrimSpace(githubUser)
	var failures []error
	changed := false
	accepted := map[string]bool{}
	for _, r := range reports {
		if r.Status != "" {
			failures = append(failures, fmt.Errorf("%s: %s", r.Hive, r.Status))
			continue
		}
		p, _ := set.Find(r.Profile)
		if newProfiles[r.Profile] {
			base, err := hivectl.HubHTTPBase(p.Hub)
			if err != nil {
				failures = append(failures, err)
				continue
			}
			reg, err := deps.registrar.Register(cmd.Context(), base, githubUser)
			if err == nil && strings.TrimSpace(reg.RegistrationToken) == "" {
				err = alreadyRegisteredError(githubUser, base, reg.Message, p.Name, p.Hub)
			}
			if err != nil {
				failures = append(failures, fmt.Errorf("%s: enrollment pending: %w", r.Hive, err))
				continue
			}
			p.RegistrationToken, p.ContributorID = strings.TrimSpace(reg.RegistrationToken), strings.TrimSpace(reg.ContributorID)
		}
		ok, err := set.ApplyPreset(r.Profile, id, r.allCurrent, r.Additions, nil)
		if err != nil {
			return err
		}
		changed = changed || ok || newProfiles[r.Profile]
		if cmd.Flags().Changed("follow-new-repos") {
			ok, err := set.SetFollowNewRepositories(r.Profile, follow)
			if err != nil {
				return err
			}
			changed = changed || ok
		}
		accepted[r.Profile] = true
	}
	// Drop staged, unsuccessful registrations. Never project an empty token or
	// an unrestricted placeholder into the relay. Successful profiles survive
	// partial failures; a retry reuses them rather than rotating credentials.
	kept := append([]hivectl.Profile(nil), set.Profiles[:originalCount]...)
	for _, p := range set.Profiles[originalCount:] {
		if accepted[p.Name] {
			kept = append(kept, p)
		}
	}
	set.Profiles = kept
	if p, _ := set.Find(set.Active); p == nil {
		set.Active = ""
		if len(kept) > 0 {
			set.Active = kept[0].Name
		}
	}
	if len(accepted) > 0 {
		// Do not mark an incomplete review as fully adopted. Retrying still
		// reports unresolved targets, while successful credentials are reused.
		if len(failures) == 0 && set.RecordPresetReview(id, source, revision, deps.now().UTC().Truncate(time.Second)) {
			changed = true
		}
	}
	if changed {
		if err := commit(deps, set); err != nil {
			return err
		}
		if err := reloadHivesAfterEdit(cmd, deps); err != nil {
			return err
		}
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Saved %d ready Hive(s). Retry this command to review unresolved targets; ready credentials are reused.\n", len(accepted))
	}
	return errors.Join(failures...)
}

func presetProfileName(set *hivectl.ProfileSet, origin string) string {
	base := hivectl.PresetReviewID(strings.TrimPrefix(strings.TrimPrefix(origin, "https://"), "http://"))
	if len(base) > 40 {
		base = base[:40]
	}
	name := base
	for n := 2; ; n++ {
		if p, _ := set.Find(name); p == nil {
			return name
		}
		name = fmt.Sprintf("%s-%d", base, n)
	}
}

// expandPresetOnboard resolves hub membership as data, never recursively, and
// keeps the publisher's original revision/provenance separate from discovery.
func (e *commandEnv) expandPresetOnboard(cmd *cobra.Command, preset *hivectl.Preset, id string) (*hivectl.Preset, []presetHiveReport) {
	expanded := *preset
	expanded.Hubs = nil
	expanded.Hives = append([]hivectl.PresetHive(nil), preset.Hives...)
	seen := map[string]bool{}
	for _, hive := range expanded.Hives {
		origin, _ := hivectl.NormalizeHiveID(hive.URL)
		seen[origin] = true
	}
	var reports []presetHiveReport
	for _, hub := range preset.Hubs {
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Hub source: %s (current membership only)\n", hub.URL)
		members, err := e.presetHubMembers(cmd, hub.URL)
		if err != nil {
			reports = append(reports, presetHiveReport{Preset: id, Hive: hub.URL, Status: "hub discovery unavailable: " + err.Error()})
			continue
		}
		wanted := map[string]bool{}
		for _, raw := range hub.Hives.URLs {
			origin, _ := hivectl.NormalizeHiveID(raw)
			wanted[origin] = true
		}
		for _, origin := range members {
			selected := hub.Hives.AllCurrent || wanted[origin]
			delete(wanted, origin)
			if selected && !seen[origin] {
				if len(expanded.Hives) >= hivectl.MaxPresetEntries {
					reports = append(reports, presetHiveReport{Preset: id, Hive: hub.URL, Status: "hub discovery unavailable: expanded preset exceeds Hive limit"})
					break
				}
				seen[origin] = true
				expanded.Hives = append(expanded.Hives, hivectl.PresetHive{URL: origin, Repositories: hivectl.PresetRepoSelector{AllCurrent: true}})
			}
		}
		for origin := range wanted {
			reports = append(reports, presetHiveReport{Preset: id, Hive: origin, Status: "not advertised by hub " + hub.URL + ", skipped"})
		}
	}
	return &expanded, reports
}

// presetHubMembers reads a hub's federation registry through the shared
// hivectl.DiscoverHubHives reader (GET /api/hives, no redirects, capped body,
// no credentials) and adds the preset-specific URL safety pass: HTTPS, or
// loopback HTTP for local development, and no embedded credentials. One
// unsafe advertised origin rejects the whole hub rather than silently
// narrowing its membership.
func (e *commandEnv) presetHubMembers(cmd *cobra.Command, hub string) ([]string, error) {
	hives, err := hivectl.DiscoverHubHives(cmd.Context(), &http.Client{Timeout: e.options.timeout}, hub)
	if err != nil {
		return nil, err
	}
	members := make([]string, 0, len(hives))
	for _, hive := range hives {
		candidate := &hivectl.Preset{Version: hivectl.PresetVersion, Name: "hub discovery", Hives: []hivectl.PresetHive{{URL: hive.ID, Repositories: hivectl.PresetRepoSelector{AllCurrent: true}}}}
		if err := candidate.Validate(); err != nil {
			return nil, err
		}
		members = append(members, hive.ID)
	}
	return members, nil
}

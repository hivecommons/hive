package main

import (
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/dashboard"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/governor"
	"github.com/hivecommons/hive/pkg/hub"
	"github.com/hivecommons/hive/pkg/tracing"
)

func (w *spokeWire) wireHubHeartbeat() {
	// Start hub heartbeat push if configured (env var or config)
	w.hubURL = w.cfg.Hub.URL
	if envHub := os.Getenv("HIVE_HUB_URL"); envHub != "" {
		w.hubURL = envHub
		w.cfg.Hub.Enabled = true
		w.cfg.Hub.URL = envHub
	}
	if envCluster := os.Getenv("HIVE_CLUSTER_ID"); envCluster != "" {
		w.cfg.Hub.ClusterID = envCluster
	}
	if w.cfg.Hub.Enabled && w.hubURL != "" {
		// Heartbeat cadence is INDEPENDENT of the governor eval interval. It was
		// previously tied to w.cfg.Governor.EvalIntervalS, so a low-ACMM hive
		// (which evaluates infrequently by design — e.g. ~10 min at L2) beat the
		// hub only every ~10 min. The hub marks a hive stale after
		// heartbeatHealthStaleness (5 min), so such hives showed a gray/stale
		// dot for half of every cycle despite being perfectly healthy. Beat on a
		// fixed interval comfortably under that 5-min threshold so every hive,
		// regardless of ACMM level, stays fresh on the hub.
		const heartbeatSendInterval = 2 * time.Minute
		// Publish the collect-independent identity BEFORE the loop starts, so
		// this spoke can report liveness even if its very first collects time
		// out. collect() below reaches api.github.com (owner-token validation,
		// and it shares the pass that enumerates issues/PRs for MTTR), which on
		// a hive with real repos routinely exceeds the collect budget right
		// after a restart. Without this, such a spoke sent NOTHING and read
		// OFFLINE on the hub while being perfectly healthy.
		hub.PublishHeartbeatIdentity(
			w.cfg.HiveID,
			w.cfg.Project.Org,
			w.cfg.Project.PrimaryRepo,
			w.cfg.Project.Repos,
			w.reporterName,
			w.processStartedAt.UTC().Format(time.RFC3339),
			gitShort,
		)
		go hub.StartHeartbeat(w.ctx, w.hubURL, w.buildHeartbeatPayload, heartbeatSendInterval, w.logger,
			hub.RestartSpokeCallback(w.handleHubRestart),
			hub.UpgradeCallback(w.handleHubUpgrade),
			hub.GitHubAppConfigCallback(w.handleHubGitHubAppConfig),
			hub.HubBannerCallback(w.handleHubBanner),
			hub.VisibilityCallback(w.handleHubVisibility),
			hub.SwitchBranchCallback(w.handleHubSwitchBranch),
			hub.AuthorizedUsersCallback(w.handleHubAuthorizedUsers),
			hub.ProjectConfigCallback(w.handleHubProjectConfig),
			hub.GatewayConfigCallback(w.handleHubGatewayConfig))

		go hub.StartTaskStatusPush(w.ctx, w.hubURL, func() *hub.TaskStatusPayload {
			reg, active := w.dashSrv.ContributorSummary()
			lb := w.dashSrv.LeaderboardForHub()
			out := make([]hub.LeaderboardEntry, len(lb))
			for i, e := range lb {
				out[i] = hub.LeaderboardEntry{
					GitHubUsername: e.GitHubUsername,
					AvatarURL:      e.AvatarURL,
					TrustTier:      e.TrustTier,
					TasksCompleted: e.TasksCompleted,
					TasksFailed:    e.TasksFailed,
					Active:         e.Active,
					CurrentTask:    e.CurrentTask,
				}
			}
			return &hub.TaskStatusPayload{
				HiveID:       w.cfg.HiveID,
				Leaderboard:  out,
				Contributors: hub.ContributorSummary{Registered: reg, Active: active},
			}
		}, w.logger)
	}

}

func (w *spokeWire) handleHubGitHubAppConfig(ghCfg *hub.HeartbeatGitHubAppConfig) {
	w.logger.Info("received github app config via heartbeat",
		"app_id", ghCfg.AppID,
		"installation_id", ghCfg.InstallationID,
		"has_key", ghCfg.PrivateKey != "",
	)

	// WRITE-PATH GUARD. Compute the identity this push WOULD produce and
	// refuse the whole delivery if it is internally inconsistent.
	//
	// This is the guard the 2026-07-31 incident needed. That push carried
	// the GHE app_id and slug with no api_url; the adoption below applies
	// each field independently under an "empty means unchanged" contract,
	// so seven public-GitHub hives took the GHE App ID, kept api_url: "",
	// and every token request 404'd. Refusing the whole delivery leaves
	// those hives on their previous, working identity instead of a half of
	// two identities.
	//
	// Rejection is loud and repeats on every beat: the hub keeps pushing
	// until the spoke reports back, so a silent skip would be an invisible
	// permanent stall. There is no auto-repair here — the fix is on the
	// hub, in clusters.json.
	if prospective := prospectiveGitHubIdentity(w.cfg.GitHub, ghCfg); prospective != nil {
		if err := config.RejectIdentitySet(*prospective); err != nil {
			w.logger.Error("REFUSING hub github app config: the pushed identity set is inconsistent and would half-apply — nothing was changed",
				"error", err,
				"pushed_app_id", ghCfg.AppID,
				"pushed_app_slug", ghCfg.AppSlug,
				"current_app_id", w.cfg.GitHub.AppID,
				"current_api_url", w.cfg.GitHub.APIURL,
				"current_base_url", w.cfg.GitHub.BaseURL,
				"remedy", "correct github_app_id/github_app_slug/github_api_url/github_base_url for this cluster on the hub",
			)
			return
		}
	}

	// Write the delivered key to the path that NAMES the App it belongs
	// to, not to a generic filename.
	//
	// /data/gh-app-key.pem carries no evidence of which App signed it, so
	// a key delivered for one App silently becomes "the key" for whatever
	// app_id the config later claims. On 2026-07-31 that is exactly what
	// happened: all 33 heartbeat-only-cluster spokes had key_file pinned to the generic
	// path holding the GHE key, so correcting app_id to the public App
	// still produced 404 Integration not found — the right key was
	// already on disk at gh-app-key-3568013.pem and unreachable, because
	// an explicit key_file short-circuits resolveAppKeyFile before the
	// per-app-id lookup runs.
	//
	// Deriving the filename from the app_id makes that mismatch
	// unrepresentable: a key can only be found under the App it was
	// delivered for. Falls back to the generic path when the delivery
	// names no App, so a key is never dropped on the floor.
	keyPath := deliveredKeyPath(ghCfg.AppID)
	// keyChanged gates dropping the cached installation token below: a
	// token minted under the previous key is invalid the moment the key
	// is replaced, but a redelivery of the SAME key must not throw away a
	// perfectly good token every heartbeat.
	keyChanged := false
	if ghCfg.PrivateKey != "" {
		// Fingerprint before and after so the key rotation is auditable
		// from the spoke's own logs. Fingerprints only — the key itself
		// is never logged.
		beforeFP, _ := config.AppKeyFingerprintFromFile(keyPath)
		if err := os.WriteFile(keyPath, []byte(ghCfg.PrivateKey), appKeys.FileMode); err != nil {
			w.logger.Error("failed to write github app key from heartbeat", "error", err)
			return
		}
		// os.WriteFile does NOT re-apply the mode to a file that already
		// exists, so a key written by an older build (or restored from a
		// looser-moded source) would keep its old permissions forever.
		// Chmod unconditionally so every path converges on 0600.
		if err := os.Chmod(keyPath, appKeys.FileMode); err != nil {
			w.logger.Warn("could not tighten github app key permissions", "path", keyPath, "error", err)
		}
		afterFP, _ := config.AppKeyFingerprintFromFile(keyPath)
		keyChanged = afterFP != "" && afterFP != beforeFP
		w.logger.Info("github app private key written via heartbeat",
			"path", keyPath,
			"from_fingerprint", beforeFP,
			"to_fingerprint", afterFP,
			"key_changed", keyChanged,
		)
		if keyChanged {
			// Invalidate before the new client is built, so nothing can
			// read the dead token out of the shared on-disk cache in
			// between. Agents read that file directly.
			w.appAuth.DropCachedToken()
		}
	}

	// Persist the fleet's ADDITIONAL App keys — every OTHER App's key,
	// keyed by app_id — so this spoke can sign for the App it is actually
	// configured as even when that is NOT its cluster's default. This is
	// the both-keys fix: a github.com hive on a GHE cluster now receives
	// and stores the github.com key here, and resolveAppKeyFile selects it
	// by matching w.cfg.GitHub.AppID.
	//
	// Written to distinct /data/gh-app-key-<appid>.pem files, so they never
	// collide with the primary /data/gh-app-key.pem above. Writing one that
	// matches our OWN app_id must take effect immediately: flip keyChanged
	// so the client is rebuilt below, exactly as a primary-key change does.
	// applyDeliveredPerAppKey writes ONE (app_id, key) pair to its
	// per-app-id file and reports whether that changed the key we
	// ourselves sign with. Shared by the (now-inert) AdditionalKeys loop
	// and the targeted SecondaryKey delivery below so both write through
	// identical code — the alternative is two copies of an atomic 0600
	// write, one of which eventually loses a guard.
	applyDeliveredPerAppKey := func(kind string, appID int64, privateKey string) {
		if privateKey == "" || appID <= 0 {
			return
		}
		perAppPath := appKeys.PerAppIDKeyPath(appID)
		beforeFP, _ := config.AppKeyFingerprintFromFile(perAppPath)
		fp, err := appKeys.WritePerAppIDKey(appID, privateKey)
		if err != nil {
			w.logger.Error("failed to write "+kind+" github app key from heartbeat",
				"app_id", appID, "error", err)
			return
		}
		changed := fp != "" && fp != beforeFP
		w.logger.Info(kind+" github app private key written via heartbeat",
			"app_id", appID,
			"path", perAppPath,
			"from_fingerprint", beforeFP,
			"to_fingerprint", fp,
			"key_changed", changed,
		)
		// If this key is for the App we ourselves authenticate as, it is
		// now the key resolveAppKeyFile will pick — treat it like a
		// primary-key rotation so the client rebuild below uses it.
		if changed && appID == w.cfg.GitHub.AppID {
			keyChanged = true
			w.appAuth.DropCachedToken()
		}
	}
	for _, ak := range ghCfg.AdditionalKeys {
		applyDeliveredPerAppKey("additional", ak.AppID, ak.PrivateKey)
	}

	// The OPTIONAL SECOND App key (#4815), delivered targeted at this
	// hive alone rather than broadcast. It lands in the same
	// /data/gh-app-key-<appid>.pem namespace the spoke has always used,
	// so heldPerAppIDKeyFingerprints reports it back on the next beat
	// (which is what stops the hub re-pushing it) and the Forge App tab
	// renders it, both with no further change. nil for every hive with no
	// second App.
	if ghCfg.SecondaryKey != nil {
		applyDeliveredPerAppKey("secondary", ghCfg.SecondaryKey.AppID, ghCfg.SecondaryKey.PrivateKey)
	}

	// Adopt a hub-delivered app_id only when it names a REAL App. Zero
	// means "not speaking to this field"; the placeholder sentinel is
	// what a pre-provisioned hive already carries, so re-adopting it
	// would overwrite a good app_id with a non-App on any heartbeat
	// that echoed the original seed back.
	if ghCfg.AppID != 0 && ghCfg.AppID != config.PlaceholderAppID {
		w.cfg.GitHub.AppID = ghCfg.AppID
	}
	// A zero installation_id means "the hub is not speaking to this
	// field", not "clear it". The cluster-wide key reconcile repairs the
	// KEY on hives whose installation_id is already correct (and which
	// the hub does not track); assigning zero here would blank a working
	// value and turn a key-only fault into a total auth outage.
	if next, cleared := nextInstallationID(w.cfg.GitHub.InstallationID, ghCfg); cleared {
		// The banner and the hub must flip to not-installed NOW, not
		// when the cached token dies an hour from now. Same config-truth
		// rule as startup.
		w.dashSrv.SetGitHubAppRequired(true)
		w.dashSrv.SetGitHubAppState(github.AppStateNotInstalled.String())
		w.logger.Info("clearing github app installation_id on operator request",
			"was", w.cfg.GitHub.InstallationID)
		w.cfg.GitHub.InstallationID = next
	} else {
		w.cfg.GitHub.InstallationID = next
	}
	// Deliberately NOT `w.cfg.GitHub.KeyFile = keyPath`.
	//
	// key_file is DERIVABLE from app_id (resolveAppKeyFile prefers
	// /data/gh-app-key-<app_id>.pem, the only key correct by
	// construction). Persisting the path turns a derived value into a
	// stored one that outlives the App it was derived for: once written,
	// it short-circuits resolveAppKeyFile on every later boot, so a
	// corrected app_id keeps signing with the previous App's key. That is
	// what left all 33 heartbeat-only-cluster spokes pinned to the GHE key.
	//
	// Leaving it empty lets derivation run every time, so the key always
	// tracks the App actually in effect. An operator-set key_file still
	// wins — that override is intentional, for a hive whose App this
	// build does not know (e.g. a hive on a third App ID with a key at a
	// bespoke path).
	// Same "empty means unchanged" contract as installation_id: adopting
	// an empty slug would blank a working install link.
	if ghCfg.AppSlug != "" && w.cfg.GitHub.AppSlug != ghCfg.AppSlug {
		w.logger.Info("adopting github app slug from hub",
			"was", w.cfg.GitHub.AppSlug, "now", ghCfg.AppSlug)
		w.cfg.GitHub.AppSlug = ghCfg.AppSlug
	}
	// Adopt the forge URLs from the SAME delivery as the App above.
	// prospectiveGitHubIdentity already validated all four fields
	// together, so reaching here means the complete set is coherent —
	// applying the App without its URLs would undo that check by leaving
	// the spoke pointed at the previous forge.
	//
	// Same "empty means unchanged" contract: empty is the correct steady
	// state for a public hive, so it can never be read as "blank this".
	if ghCfg.APIURL != "" && w.cfg.GitHub.APIURL != ghCfg.APIURL {
		w.logger.Info("adopting github api url from hub",
			"was", w.cfg.GitHub.APIURL, "now", ghCfg.APIURL)
		w.cfg.GitHub.APIURL = ghCfg.APIURL
	}
	if ghCfg.BaseURL != "" && w.cfg.GitHub.BaseURL != ghCfg.BaseURL {
		w.logger.Info("adopting github base url from hub",
			"was", w.cfg.GitHub.BaseURL, "now", ghCfg.BaseURL)
		w.cfg.GitHub.BaseURL = ghCfg.BaseURL
	}

	// Persist the adopted App IDENTITY to the PVC overlay, exactly as the
	// claimed-project-config callback persists what it adopts.
	//
	// Without this the adoption lived only in memory. The key files are
	// written to /data (durable), but app_id/app_slug/installation_id were
	// not, so on every pod restart the entrypoint re-merged the ConfigMap
	// seed and the spoke reverted to whatever App it was provisioned with —
	// silently undoing a completed repair and making the hub's push look
	// like it had never happened. That is how a GHE hive kept re-appearing
	// with the github.com app_id and an empty slug across restarts.
	//
	// Saved even when the App is not yet usable (installation_id still 0):
	// the corrected app_id and slug are precisely what the owner needs on
	// disk so the dashboard renders a working install link BEFORE they have
	// installed anything.
	if err := w.cfg.Save(); err != nil {
		w.logger.Error("failed to persist github app config from heartbeat", "error", err)
	}

	// Resolve the key file the same way startup does, so a hive whose
	// only correct key arrived as an ADDITIONAL per-app-id key (no
	// primary key_file configured — the exact heartbeat-only-cluster state) still finds
	// it: resolveAppKeyFile prefers /data/gh-app-key-<appid>.pem for the
	// app_id we now claim. An explicit key_file still wins outright.
	rebuildKeyFile := appKeys.Resolve(w.cfg.GitHub.KeyFile, os.Getenv("GH_APP_KEY_FILE"), w.cfg.GitHub.AppID)
	if w.cfg.GitHub.HasUsableApp() && rebuildKeyFile != "" {
		newAppAuth, err := github.NewAppAuth(w.cfg.GitHub.AppID, w.cfg.GitHub.InstallationID, rebuildKeyFile, w.logger, w.cfg.GitHub.ResolvedAPIURL())
		if err != nil {
			w.logger.Error("github app auth init via heartbeat failed", "error", err)
			return
		}
		// Deliberately NOT persisted. rebuildKeyFile is the RESOLVED
		// path, and writing a resolved value back into config is what
		// converts a derivation into a pin: the next boot reads it as an
		// explicit key_file, short-circuits resolveAppKeyFile, and keeps
		// using this App's key even after app_id changes. Re-resolving on
		// every use costs a stat and cannot go stale.
		// Hub-delivered creds can carry a wrong installation_id just as
		// easily as a hand-edited config; correct (and persist) it
		// before building a client that would 403 on every write.
		healGitHubAppInstallation(w.ctx, newAppAuth, w.cfg, w.logger)
		newClient := github.NewClientFromAppWithBotLogin(newAppAuth, w.cfg.Project.Org, w.cfg.Project.Repos, w.logger, w.cfg.GitHub.BotLogin())
		if len(w.cfg.Governor.Labels.Exempt) > 0 {
			newClient.SetExemptLabels(w.cfg.Governor.Labels.Exempt)
			newClient.SetAutoMergeLabel(normalizedAutoMergeLabel(w.cfg.Governor.Labels.AutoMerge))
		}
		newClient.SetIssueFilter(w.cfg.Project.IssueFilter)
		w.ghClient = newClient
		w.installMutationBoundary(w.ghClient)
		w.appAuth = newAppAuth
		w.agentMgr.SetAppAuth(newAppAuth)
		// Immediate per-agent token delivery: hosted spokes get their
		// App creds via this heartbeat path AFTER agents have already
		// launched (with empty 0-byte caches), so waiting for the next
		// 40-minute tick guarantees a window of gh 401s (#4072).
		go w.agentMgr.RefreshAgentTokens(w.ctx)
		w.dashSrv.UpdateGitHubClient(newClient, newAppAuth)
		w.dashSrv.SetGitHubAppRequired(false)
		w.dashSrv.ClearPendingGitHubAppInstall()
		w.logger.Info("github app configured via heartbeat delivery",
			"app_id", w.cfg.GitHub.AppID,
			"installation_id", w.cfg.GitHub.InstallationID,
		)
	}

}

func (w *spokeWire) handleHubBanner(banner *hub.HubBanner) {
	if banner == nil {
		w.dashSrv.ClearHubBanner()
		return
	}
	w.dashSrv.SetHubBanner(banner.ID, banner.Message, banner.Color)

}

func (w *spokeWire) handleHubVisibility(isPublic bool) {
	if w.cfg.Hub.IsPublic != isPublic {
		w.logger.Info("hub overrode visibility via heartbeat",
			"was", w.cfg.Hub.IsPublic, "now", isPublic)
		w.cfg.Hub.IsPublic = isPublic
	}

}

func (w *spokeWire) handleHubSwitchBranch(tag string) {
	// Branch switch delivered via heartbeat (the hub couldn't reach
	// this cluster over kubectl). Patch our OWN deployment image via
	// the in-cluster K8s API — the pod has no kubectl binary, but its
	// SA holds the hive-self-upgrade role (patch on deployment/hive).
	// K8s then rolls the pod onto the new tag.
	image := "ghcr.io/hivecommons/hive:" + tag
	if err := hub.SwitchImageSelf(w.logger, image); err != nil {
		w.logger.Warn("branch switch via heartbeat failed", "tag", tag, "image", image, "error", err)
		return
	}

}

func (w *spokeWire) handleHubAuthorizedUsers(users []string, names map[string]string) {
	// The hub delivered its authoritative access list. Reconcile our
	// login allowlist so Manage Access grants take effect on this
	// heartbeat-only spoke without any kubectl push. The dashboard reads
	// w.cfg.Dashboard.AuthorizedUsers live on each login, so updating it in
	// place is enough. Only log when it actually changes to avoid noise.
	if !sameStringSlice(w.cfg.Dashboard.AuthorizedUsers, users) {
		w.logger.Info("authorized users updated from hub heartbeat",
			"was", len(w.cfg.Dashboard.AuthorizedUsers), "now", len(users))
		w.cfg.Dashboard.AuthorizedUsers = users
	}
	// AuthorizedUserNames is purely cosmetic (see its doc) — it never
	// gates sign-in, so it's fine to just take whatever the hub sent
	// (including nil, which means "no names known") without the
	// same-value guard above.
	w.cfg.Dashboard.AuthorizedUserNames = names

}

func (w *spokeWire) handleHubProjectConfig(pc *hub.HeartbeatProjectConfig) {
	// The hub assigned this (previously placeholder) hive a real project.
	// Reconcile our running project config so agents work the claimed
	// org/repos at the claimed maturity level. This is the ONLY delivery
	// channel on heartbeat-only clusters (the heartbeat-only cluster) — no kubectl push is
	// possible. The hub keeps sending this every beat until we report the
	// matching project back, so an idempotent no-op when already matched
	// is expected and cheap.
	if pc == nil {
		return
	}
	// A URL-only push (org empty, dashboard_url set) delivers the vanity
	// dashboard URL to an already-claimed hive whose meta project is stale/
	// empty on the hub — we must still adopt+report it, or the hub keeps
	// showing the raw placeholder host forever. Handle it BEFORE the
	// org-empty bail below, which exists so an empty project never blanks a
	// working config: with no org there is nothing to reconcile except the
	// URL, so adopt it, persist, and return without touching the project.
	if pc.Org == "" {
		if pc.DashboardURL != "" && w.cfg.Hub.DashboardURL != pc.DashboardURL {
			w.logger.Info("adopting vanity dashboard URL from hub heartbeat (url-only push)",
				"was", w.cfg.Hub.DashboardURL, "now", pc.DashboardURL)
			w.cfg.Hub.DashboardURL = pc.DashboardURL
			if err := w.cfg.Save(); err != nil {
				w.logger.Error("failed to save adopted vanity dashboard URL", "error", err)
			}
		}
		return
	}
	if issue := config.ValidateProjectRepoTargets(pc.Org, pc.Repos, pc.PrimaryRepo, w.cfg.GitHub.HostLabel()); issue != nil {
		w.logger.Error("REFUSING hub project config: repo target is misconfigured — project left unchanged",
			"error", issue.Message,
			"pushed_org", pc.Org,
			"pushed_repos", pc.Repos,
			"pushed_primary_repo", pc.PrimaryRepo,
		)
		return
	}
	curACMM := 0
	if w.cfg.ACMMLevel != nil {
		curACMM = *w.cfg.ACMMLevel
	}
	// Adopt the vanity dashboard URL delivered on claim, if any. We
	// report w.cfg.Hub.DashboardURL in our heartbeats, so once set the hub
	// registry's dashboardUrl becomes the vanity URL (not the placeholder
	// host). Track it in the already-reconciled check so a URL-only change
	// still gets applied and persisted.
	vanityMatched := pc.DashboardURL == "" || w.cfg.Hub.DashboardURL == pc.DashboardURL
	authorMatched := pc.AIAuthor == "" || w.cfg.Project.AIAuthor == pc.AIAuthor
	apiURLMatched := pc.GitHubAPIURL == "" || w.cfg.GitHub.APIURL == pc.GitHubAPIURL
	// Issue filter: nil means "the hub is not speaking to this field"
	// (mirrors AIAuthor's empty-means-keep), so the hub's every-beat
	// echo can never blank a locally configured filter.
	issueFilterMatched := pc.IssueFilter == nil || w.cfg.Project.IssueFilter.Equal(*pc.IssueFilter)
	if w.cfg.Project.Org == pc.Org &&
		sameStringSlice(w.cfg.Project.Repos, pc.Repos) &&
		w.cfg.Project.PrimaryRepo == pc.PrimaryRepo &&
		curACMM == pc.ACMMLevel &&
		authorMatched &&
		apiURLMatched &&
		issueFilterMatched &&
		vanityMatched {
		return // already reconciled
	}
	if pc.DashboardURL != "" && w.cfg.Hub.DashboardURL != pc.DashboardURL {
		w.logger.Info("adopting vanity dashboard URL from hub heartbeat",
			"was", w.cfg.Hub.DashboardURL, "now", pc.DashboardURL)
		w.cfg.Hub.DashboardURL = pc.DashboardURL
	}
	w.logger.Info("project config updated from hub heartbeat (placeholder claimed)",
		"was_org", w.cfg.Project.Org, "now_org", pc.Org,
		"repos", pc.Repos, "primary_repo", pc.PrimaryRepo,
		"acmm_level", pc.ACMMLevel)
	w.cfg.Project.Org = pc.Org
	w.cfg.Project.Repos = pc.Repos
	w.cfg.Project.PrimaryRepo = pc.PrimaryRepo
	// Only adopt a non-empty author. The hub echoes this struct back on
	// every beat, so assigning unconditionally would reset a locally
	// configured ai_author to "" each time — which is precisely what
	// kept the fleet-stats collector disabled on every hive.
	if pc.AIAuthor != "" {
		w.cfg.Project.AIAuthor = pc.AIAuthor
	}
	// Adopt a hub-delivered issue filter only when the hub actually
	// sent one (non-nil). A push without the field leaves the spoke's
	// locally configured filter untouched — the org/repos assignments
	// above never wipe it either, so a local filter SURVIVES claim
	// delivery. A non-nil but EMPTY filter is an explicit clear.
	if pc.IssueFilter != nil && !w.cfg.Project.IssueFilter.Equal(*pc.IssueFilter) {
		w.logger.Info("adopting issue filter from hub heartbeat",
			"require_labels", pc.IssueFilter.RequireLabels)
		w.cfg.Project.IssueFilter = *pc.IssueFilter
	}
	// Adopt a GitHub Enterprise API URL when the hub sends one. Empty
	// means "leave mine alone" — the spoke's own default is already
	// api.github.com, so this never clobbers a working config.
	if pc.GitHubAPIURL != "" && w.cfg.GitHub.APIURL != pc.GitHubAPIURL {
		// WRITE-PATH GUARD, mirroring the App-config callback: an api_url
		// that names a different forge than our app_id is the same
		// half-applied identity arriving from the other direction. Skip
		// only this field — the org/repos/ACMM adoption around it is
		// unrelated and must still land.
		prospective := w.cfg.GitHub
		prospective.APIURL = pc.GitHubAPIURL
		if err := config.RejectIdentitySet(prospective); err != nil {
			w.logger.Error("REFUSING hub GitHub API URL: it does not match this hive's app_id and would half-apply an identity — api_url left unchanged",
				"error", err,
				"pushed_api_url", pc.GitHubAPIURL,
				"current_api_url", w.cfg.GitHub.APIURL,
				"current_app_id", w.cfg.GitHub.AppID,
				"remedy", "correct github_api_url/github_app_id for this cluster on the hub",
			)
		} else {
			w.logger.Info("adopting GitHub API URL from hub heartbeat",
				"was", w.cfg.GitHub.APIURL, "now", pc.GitHubAPIURL)
			w.cfg.GitHub.APIURL = pc.GitHubAPIURL
		}
	}
	level := pc.ACMMLevel
	w.cfg.ACMMLevel = &level

	// Re-sync the GitHub client that caches the repo list (mirrors the
	// config-watcher reload path). The issue filter is cached the same
	// way, so re-install it too — a hub-delivered filter must take
	// effect on the next enumeration, not the next restart.
	w.ghClient.SetRepos(w.cfg.Project.Repos)
	w.ghClient.SetIssueFilter(w.cfg.Project.IssueFilter)

	// Persist to the PVC overlay so the claim survives a pod restart
	// (config save writes the overlay hive.yaml, same as level switches).
	if err := w.cfg.Save(); err != nil {
		w.logger.Error("failed to save claimed project config", "error", err)
	}

}

func (w *spokeWire) handleHubGatewayConfig(gw *hub.HeartbeatGatewayConfig) {
	// The hub funded an OpenRouter gateway on this hive's behalf (scan-to-
	// fund from My Hives) and delivered it over the heartbeat channel — the
	// only path that reaches a firewalled/heartbeat-only spoke (the heartbeat-only cluster). We
	// store the key in our OWN per-gateway secret-file store and create the
	// "openrouter" gateway. The hub drains the delivery after sending, so
	// this fires once per fund; the key value is never logged.
	if gw == nil || gw.Key == "" {
		return
	}
	if err := w.dashSrv.ApplyDeliveredGateway(gw.Name, gw.Kind, gw.Endpoint, gw.DefaultModel, gw.Key); err != nil {
		w.logger.Error("failed to apply hub-delivered gateway", "gateway", gw.Name, "error", err)
	}

}

func (w *spokeWire) heartbeatFleetStats() (*int, *int, *int, string) {
	var prsMerged, prsRejected, cvesClosed *int
	collectedAt := ""
	if fc, ok := w.fleetStatsCollector.Snapshot(); ok {
		m, rj, cv := fc.PRsMerged, fc.PRsRejected, fc.CVEsClosed
		prsMerged, prsRejected, cvesClosed = &m, &rj, &cv
		if t := w.fleetStatsCollector.CollectedAt(); !t.IsZero() {
			collectedAt = t.UTC().Format(time.RFC3339)
		}
	}
	return prsMerged, prsRejected, cvesClosed, collectedAt
}

func (w *spokeWire) heartbeatRepoActivity() ([]hub.RepoActivityWire, string, int, int) {
	if asnap, ok := w.activityCollector.Snapshot(); ok {
		collectedAt := ""
		if t := w.activityCollector.CollectedAt(); !t.IsZero() {
			collectedAt = t.UTC().Format(time.RFC3339)
		}
		return buildRepoActivityWire(asnap.Repos), collectedAt, asnap.WindowHours, asnap.CountWindowHours
	}
	return nil, "", 0, 0
}

func (w *spokeWire) heartbeatBudgetWindow() (*int64, *int64, *bool, string, string) {
	budget := w.gov.GetBudget()
	limit := budget.WeeklyLimit
	ignored := budget.IgnoreAll
	if start, end, ok := w.gov.BudgetWindow(); ok {
		spend := budget.CurrentSpend
		return &spend, &limit, &ignored, start.UTC().Format(time.RFC3339), end.UTC().Format(time.RFC3339)
	}
	return nil, &limit, &ignored, "", ""
}

func heartbeatBudgetState(govState governor.State) (*bool, *int) {
	if govState.LastEval.IsZero() {
		return nil, nil
	}
	exhausted := govState.BudgetExhausted
	violations := govState.SLAViolations
	return &exhausted, &violations
}

func (w *spokeWire) buildHeartbeatPayload() *hub.HeartbeatPayload {
	if !w.cfg.Hub.Enabled {
		return nil
	}
	statuses := w.agentMgr.AllStatuses()
	govState := w.gov.GetState()
	currentMode := strings.ToLower(string(govState.Mode))
	agents := make([]hub.AgentSummary, 0, len(statuses))
	for name, proc := range statuses {
		mode := ""
		if ac, ok := w.cfg.Agents[name]; (ok && ac.OnDemand) || w.onDemandFromPack[name] {
			mode = "on_demand"
		}
		agents = append(agents, hub.NewAgentSummary(name, string(proc.State), mode,
			hub.AgentActivityFor(w.agentMgr, w.cfg, govState, currentMode, name, proc, w.onDemandFromPack)))
	}
	acmmLvl := 0
	if w.cfg.ACMMLevel != nil {
		acmmLvl = *w.cfg.ACMMLevel
	}
	prsMerged, prsRejected, cvesClosed, fleetStatsCollectedAt := w.heartbeatFleetStats()
	repoActivity, repoActivityCollectedAt, repoActivityWindowHours, repoActivityCountWindowHours := w.heartbeatRepoActivity()
	// Count agents with a method/model assigned for the hub's
	// user-journey stage detection. Always a non-nil pointer from a
	// spoke new enough to compute it, so the hub can distinguish
	// "genuinely zero agents configured" from "old spoke, unknown".
	agentsWithModel := w.agentMgr.CountAgentsWithModel()

	// --- Quadrant signals ------------------------------------------
	// All read from state this spoke already maintains on an existing
	// timer: ZERO new GitHub API calls, which matters because the whole
	// fleet shares one search quota. Every one stays nil unless its
	// source has actually produced a measurement — the hub's scorer
	// reads nil as absent evidence and a zero as a genuine low score,
	// so emitting a zero for missing data would silently misinform
	// operators rather than merely lose precision.

	budgetSpend, budgetLimit, budgetIgnored, budgetWindowStartsAt, budgetWindowEndsAt := w.heartbeatBudgetWindow()
	budgetExhausted, slaViolations := heartbeatBudgetState(govState)

	// Hold comes from the cached actionable result rather than
	// govState.QueueHold: both carry the same number, but the cache is
	// a nilable pointer, so a spoke that has not yet completed (or
	// restored) a scan reports nil instead of an int zero that is
	// indistinguishable from "nothing is on hold".
	var holdTotal *int
	if act := w.lastActionable.Load(); act != nil {
		total := act.Hold.Total
		holdTotal = &total
	}

	// Planning is unavailable below ACMM L5, where AwaitingReview is
	// structurally zero rather than measured — report nil so the hub
	// does not read "no plans are blocked on a human" into a hive that
	// has no planning subsystem at all.
	//
	// architectPaused is passed false rather than resolved from agent
	// statuses: it feeds only FrontendPlanning.ArchitectPaused, which
	// this heartbeat does not send, and the resolver is unexported to
	// pkg/dashboard. Passing false cannot perturb AwaitingReview.
	var awaitingReview *int
	if planning := dashboard.BuildPlanning(w.beadStores, false, acmmLvl); planning.Available {
		n := planning.AwaitingReview
		awaitingReview = &n
	}

	// Contributor-relay tasks over the trailing 7d, summed from the
	// spoke's own 168 hourly buckets. nil until the store exists; a
	// zero from an existing store is a real "no contributor finished
	// anything" reading.
	var tasksCompleted7d *int
	if n, ok := w.dashSrv.TasksCompleted7d(); ok {
		tasksCompleted7d = &n
	}

	providerLimitReason, providerLimitRebuffs, providerLimitHiveWide, providerLimitAgents := hub.ProviderLimitHeartbeatFields(agents, dashboard.InferenceBudgetExceeded)
	lastWriteKickAt, kickDisposition, kickSkipReason, notWritableQueued :=
		outputFreshnessHeartbeatFields(acmmLvl, govState, agents)
	ghAppTokenStatus, ghAppTokenLastMintAt, ghAppTokenError :=
		githubAppTokenHeartbeatFields(w.cfg, w.dashSrv.GetGitHubAppPermIssue())
	ghAppErrorClass, ghAppHTTPStatus := githubAppStructuredFailure(
		w.dashSrv.GetGitHubAppState(),
		firstNonEmpty(w.dashSrv.GetGitHubAppPermIssue(), ghAppTokenError),
	)

	// Remediation-hint detectors (#5577). All three read state the
	// spoke already maintains — no new GitHub calls, no new file
	// scans on the beat path. AgentErrorStreaks is nil until the
	// token collector's first bob-recording scan completes ("not
	// measured", hub carries forward); the other two are always live
	// measurements and send [] to clear a stale carry-forward.
	agentErrorStreaks := w.tokenCollector.AgentErrorStreaks()
	consentWedged := w.agentMgr.ConsentWedgedAgents()
	noCadenceAgents := w.gov.NoCadenceAgents()

	return &hub.HeartbeatPayload{
		AgentsWithModel:      &agentsWithModel,
		BudgetCurrentSpend:   budgetSpend,
		BudgetLimit:          budgetLimit,
		BudgetWindowStartsAt: budgetWindowStartsAt,
		BudgetWindowEndsAt:   budgetWindowEndsAt,
		BudgetExhausted:      budgetExhausted,
		BudgetIgnored:        budgetIgnored,
		HoldTotal:            holdTotal,
		AwaitingReview:       awaitingReview,
		SLAViolations:        slaViolations,
		TasksCompleted7d:     tasksCompleted7d,
		AgentErrorStreaks:    agentErrorStreaks,
		ConsentWedged:        consentWedged,
		NoCadenceAgents:      noCadenceAgents,
		// Read-back for hub-funded gateways: the hub clears its pending
		// record only when it sees the gateway named here, so a lost
		// delivery is re-offered rather than dropped. Names only — the
		// key never leaves the spoke.
		GatewayNames: w.dashSrv.ConfiguredGatewayNames(),
		// Hash only, never the raw token: lets the hub verify this
		// spoke's upgrade-proof credential without reading the
		// hive-secrets secret from a cluster it may not reach
		// (pull-only). Empty when no token is configured.
		DashboardTokenHash: func() string {
			if w.cfg.Dashboard.AuthToken == "" {
				return ""
			}
			return hub.HashDashboardToken(w.cfg.Dashboard.AuthToken)
		}(),
		HiveID:            w.cfg.HiveID,
		Org:               w.cfg.Project.Org,
		AIAuthor:          w.cfg.Project.AIAuthor,
		AIAuthorEffective: w.cfg.EffectiveAIAuthor(),
		StartedAt:         w.processStartedAt.UTC().Format(time.RFC3339),
		// FD gauge (#3875): a socket leak reached 92,962 FDs and
		// self-DoSed spokes with nothing surfacing it. Report the count
		// and its rlimit every beat so the next leak is a climbing
		// number on the hub, not a manual /proc excavation.
		OpenFDs:     hub.OpenFDCount(),
		FDSoftLimit: hub.FDSoftLimit(),
		// Reporter names THIS process (the pod) so the hub can tell two
		// instances reporting as one hive apart — the pod name is the
		// hostname inside the container.
		Reporter: w.reporterName,
		// Advisory-staleness signal (mirrors StartedAt/uptime). Report the
		// last successful digest-post time only if the spoke has actually
		// posted one — a zero time is left as an empty string so the hub
		// reads it as UNKNOWN (not-advisory-mode / old spoke), never a
		// false stale alarm. The last post error rides alongside so a
		// working-App-but-failing-post hive can be flagged with its cause.
		AdvisoryLastPostedAt: func() string {
			postedAt, _, _ := w.dashSrv.AdvisoryState()
			if postedAt.IsZero() {
				return ""
			}
			return postedAt.UTC().Format(time.RFC3339)
		}(),
		AdvisoryError: func() string {
			_, _, errMsg := w.dashSrv.AdvisoryState()
			return errMsg
		}(),
		// Digest SHAPE: how many findings went out, and how many the
		// top-N cap withheld. The hub renders the pair so a capped
		// digest never reads as the complete picture.
		AdvisoryFindingCount: func() int {
			findings, _ := w.dashSrv.AdvisoryCounts()
			return findings
		}(),
		AdvisoryOverflowCount: func() int {
			_, overflow := w.dashSrv.AdvisoryCounts()
			return overflow
		}(),
		// Inference-backend auth-failure signal (repeated 401s from a
		// stale gateway key). Reported as its own field so the hub can
		// raise a dedicated inference-auth alert whose ROOT cause an
		// operator sees directly — distinct from the advisory-staleness
		// pill AdvisoryError also trips. Empty when inference auth is
		// healthy or the hive routes to no inference backend; self-clears
		// on the next successful inference call.
		InferenceAuthError: func() string {
			errMsg, _ := w.dashSrv.InferenceAuthState()
			return errMsg
		}(),
		ProviderLimitReason:     providerLimitReason,
		ProviderLimitRebuffs:    providerLimitRebuffs,
		ProviderLimitHiveWide:   providerLimitHiveWide,
		ProviderLimitAgents:     providerLimitAgents,
		LastWriteCapableKickAt:  lastWriteKickAt,
		LastKickDisposition:     kickDisposition,
		LastKickSkipReason:      kickSkipReason,
		NotWritableQueued:       notWritableQueued,
		RepoTargetMisconfigured: w.repoTargetMisconfigured(),
		RepoTargetIssue:         w.repoTargetIssueMessage(),
		Repos:                   w.cfg.Project.Repos,
		PrimaryRepo:             w.cfg.Project.PrimaryRepo,
		ACMMLevel:               acmmLvl,
		Agents:                  agents,
		Governor: hub.GovernorSummary{Mode: string(govState.Mode), Issues: govState.QueueIssues, PRs: govState.QueuePRs, WorkSource: func() string {
			if t := w.cfg.Governor.WorkSource.Type; t != "" && t != "github" {
				return t
			}
			return ""
		}()},
		// Tokens carries the spoke's authoritative cumulative token
		// total (same store the dashboard token panel and governor
		// budget read). It flows to the hub's My Hives token column so
		// heartbeat-only hives (reached via heartbeat, not hub-kubectl)
		// display real consumption. Refreshed each heartbeat, so the
		// column is as fresh as the last heartbeat. Despite the
		// "24h"-suffixed field name this is a lifetime/window total,
		// consistent with what the spoke dashboard already shows.
		Tokens24h: func() int64 {
			if w.tokenCollector == nil {
				return 0
			}
			if summary := w.tokenCollector.Summary(); summary != nil {
				return summary.TotalTokens
			}
			return 0
		}(),
		Contributors: func() hub.ContributorSummary {
			reg, active := w.dashSrv.ContributorSummary()
			return hub.ContributorSummary{Registered: reg, Active: active}
		}(),
		Leaderboard: w.leaderboardForHeartbeat(),
		// Report who has a live dashboard session so the hub can accumulate
		// per-user "time in hive". Bare usernames only — never session
		// ids/tokens (ActiveSessionUsernames guarantees this).
		ActiveSessionUsers: w.dashSrv.ActiveSessionUsernames(),
		// The honest subset of the above: users whose browser reported
		// focused, recent-input presence (see dashboard/presence.go).
		// An idle open tab appears in ActiveSessionUsers but not here.
		EngagedSessionUsers: w.dashSrv.EngagedSessionUsernames(),
		// Per-user last audit-logged real action, so the hub can tell
		// users who DO things from users who merely stay logged in.
		UserLastActions: w.dashSrv.UserLastActions(),
		Owner:           w.ownerForHeartbeat(),
		// Report the API URL we are actually running against so the hub
		// can see whether a GitHub Enterprise API URL it delivered has
		// landed. Resolved (never empty) so the hub can distinguish
		// "public github.com" from "spoke too old to report this".
		GitHubAPIURL: w.cfg.GitHub.ResolvedAPIURL(),
		Health:       w.dashSrv.HealthSummary(),
		DashboardURL: w.dashboardURLForHeartbeat(),
		SnapshotURL:  w.cfg.Hub.SnapshotURL,
		HiveType:     w.cfg.Hub.HiveType,
		ClusterID:    w.cfg.Hub.ClusterID,
		IsPublic:     w.cfg.Hub.IsPublic,
		Version:      version,
		GitHash:      gitShort,
		GitBranch:    gitBranch,
		// The image ref the Deployment tracks, read in-cluster and
		// cached. The hub cannot see it for firewalled spokes, and it
		// is the only way to distinguish a hive pinned to an immutable
		// SHA tag (which can never receive a rolling upgrade) from one
		// riding <branch>-latest. Empty off-cluster — never guessed.
		ImageRef: hub.SelfDeploymentImage(),
		// The GitHub instance this spoke actually runs against. Only
		// the spoke knows this for certain: a hive's GitHub can differ
		// from its cluster's default, so the hub cannot infer it.
		// Reported as a bare hostname via HostLabel(), which reads BOTH
		// base_url and api_url — a GHE placeholder with base_url:"" but
		// api_url: github.ibm.com must report github.ibm.com, not be
		// silently rendered as github.com in the spokes table.
		GitHubHost:               w.cfg.GitHub.HostLabel(),
		GitHubAppRequired:        w.dashSrv.IsGitHubAppRequired(),
		GitHubAppPermIssue:       w.dashSrv.GetGitHubAppPermIssue(),
		GitHubAppState:           w.dashSrv.GetGitHubAppState(),
		GitHubAppTokenStatus:     ghAppTokenStatus,
		GitHubAppTokenLastMintAt: ghAppTokenLastMintAt,
		GitHubAppTokenError:      ghAppTokenError,
		GitHubAppErrorClass:      ghAppErrorClass,
		GitHubAppHTTPStatus:      ghAppHTTPStatus,
		PendingGitHubAppInstall:  w.dashSrv.IsPendingGitHubAppInstall(),
		AutoUpgrade:              w.cfg.Hub.AutoUpgrade,
		ClusterHealth: func() *hub.HeartbeatClusterHealthReport {
			if os.Getenv("HIVE_CLUSTER_ID") == "" {
				return nil
			}
			return hub.CollectClusterHealth(w.logger)
		}(),
		PRsMerged90d:                 prsMerged,
		PRsRejected90d:               prsRejected,
		CVEsClosed:                   cvesClosed,
		FleetStatsCollectedAt:        fleetStatsCollectedAt,
		RepoActivity:                 repoActivity,
		RepoActivityCollectedAt:      repoActivityCollectedAt,
		RepoActivityWindowHours:      repoActivityWindowHours,
		RepoActivityCountWindowHours: repoActivityCountWindowHours,
		// Report WHICH App key we hold, never the key. The hub compares
		// this against its per-cluster key and pushes a correction only
		// on a mismatch, so a spoke already holding the right key costs
		// nothing and a spoke holding the wrong one self-heals.
		GitHubAppKeyFingerprint: appKeys.ReportedFingerprint(w.cfg.GitHub.KeyFile, w.cfg.GitHub.AppID),
		GitHubAppKeyPerHive:     appKeys.HasPerHiveKey(w.cfg.GitHub.KeyFile, w.cfg.GitHub.AppID),
		// Report the App this hive believes it authenticates as. The hub
		// pairs it with the fingerprint above to tell a per-hive key that
		// is WRONG for this App from one that is deliberately for another.
		GitHubAppID: w.cfg.GitHub.AppID,
		// Report the REST of the identity set too. app_id alone cannot
		// distinguish a correctly-delivered identity from a
		// half-applied one: a GHE app_id with an empty api_url looks
		// identical to the hub, and 404s on every token request. All
		// four together let the hub see the whole set.
		GitHubAppSlug:        w.cfg.GitHub.AppSlug,
		GitHubInstallationID: w.cfg.GitHub.InstallationID,
		GitHubBaseURL:        w.cfg.GitHub.BaseURL,
		// Report the fingerprint of every ADDITIONAL per-app-id key already
		// on the PVC, so the hub delivers the fleet's other App keys once
		// and then stops re-sending them.
		GitHubAppKeysHeld: appKeys.HeldFingerprints(),
		// Component reach counters (#3993, phase 2a of #3973): per
		// (component, running commit) span counts aggregated in-process,
		// exporter or not (D2) — the heartbeat is the only channel that
		// reaches every spoke, pull-only ones included (D1). nil until
		// the first span, which the hub reads as "no data", never as
		// zero reach. Capped at tracing.MaxReachComponents entries.
		ComponentReach: tracing.ReachSnapshot(),
	}

}

func (w *spokeWire) leaderboardForHeartbeat() []hub.LeaderboardEntry {
	lb := w.dashSrv.LeaderboardForHub()
	out := make([]hub.LeaderboardEntry, len(lb))
	for i, e := range lb {
		out[i] = hub.LeaderboardEntry{
			GitHubUsername: e.GitHubUsername,
			AvatarURL:      e.AvatarURL,
			TrustTier:      e.TrustTier,
			TasksCompleted: e.TasksCompleted,
			TasksFailed:    e.TasksFailed,
			Active:         e.Active,
			CurrentTask:    e.CurrentTask,
		}
	}
	return out
}

func (w *spokeWire) ownerForHeartbeat() string {
	if td, err := os.ReadFile("/data/gh-user-token"); err == nil {
		tok := strings.TrimSpace(string(td))
		if tok != "" {
			// gh-user-token is a github.com OAuth token — validate its identity against github.com,
			// not the (possibly GHE) repo host.
			if u, err := github.ValidateToken(tok, w.cfg.GitHub.OAuthAPIURL()); err == nil {
				return u.Login
			}
		}
	}
	return ""
}

func (w *spokeWire) dashboardURLForHeartbeat() string {
	if w.cfg.Hub.DashboardURL != "" {
		return w.cfg.Hub.DashboardURL
	}
	// Prefer the host our OWN Route/Ingress actually serves. The synthesised
	// "<hiveID>.<hub host>" below is only correct when this spoke is fronted by
	// the hub's wildcard domain; pull-only clusters must report their live host.
	if host := hub.SpokeServedHost(w.ctx); host != "" {
		return "https://" + host
	}
	if w.cfg.HiveID != "" && w.cfg.Hub.URL != "" {
		if u, err := url.Parse(w.cfg.Hub.URL); err == nil && u.Host != "" {
			return fmt.Sprintf("https://%s.%s", w.cfg.HiveID, u.Host)
		}
	}
	return fmt.Sprintf("http://localhost:%d", w.cfg.Dashboard.Port)
}

func (w *spokeWire) handleHubRestart() {
	if up := time.Since(w.processStartedAt); up < spokeRestartMinUptime {
		w.logger.Info("hub requested a spoke restart; ignoring — this process just started",
			"uptime", up.Round(time.Second))
		return
	}
	w.logger.Warn("hub requested a spoke restart — rolling this deployment",
		"reporter", w.reporterName)
	if err := hub.RolloutRestartSelf(w.logger); err != nil {
		// Do NOT exit here: without deployment-patch RBAC an exit would
		// restart onto the same state every delivery and look like a
		// crash-loop. The error names the missing Role instead.
		w.logger.Error("spoke restart failed: could not patch own Deployment",
			"error", err,
			"hint", "grant get/patch on deployments/hive in this namespace (hive-self-upgrade Role/RoleBinding)")
	}

}

func (w *spokeWire) handleHubUpgrade(targetSHA string) {
	const upgradeMarkerPath = "/data/upgrade-requested"

	// attemptCount carries the number of PREVIOUS failed attempts for this
	// (current_sha → target_sha) pair, read from the marker below.
	attemptCount := 0

	// Never self-upgrade to the commit we are already running. The hub
	// may instruct an upgrade to a short SHA that is a prefix of our
	// full gitShort (or vice-versa); treating that as "behind" caused a
	// crash-loop (patch → 403 → os.Exit → repeat) on hives sitting
	// exactly at HEAD. Prefix-compare so same-commit is a no-op.
	if sha1, sha2 := targetSHA, gitShort; sha1 != "" && sha2 != "" {
		n := len(sha1)
		if len(sha2) < n {
			n = len(sha2)
		}
		if strings.EqualFold(sha1[:n], sha2[:n]) {
			w.logger.Info("self-upgrade skipped: target is the running commit",
				"target", targetSHA, "current", gitShort)
			return
		}
	}

	// A previous process attempted an upgrade and we booted with the same
	// git hash, so the image did not actually change: the attempt FAILED.
	// Back off rather than retrying instantly (that was a crash-loop), but
	// do NOT latch forever — "image unchanged" is the signature of a failed
	// upgrade, not a reason to stop trying. The latch is keyed on
	// (current_sha → target_sha) and bounded to selfUpgradeMaxAttempts, so a
	// NEW target always gets a fresh budget and a transient failure (an RBAC
	// Role that showed up late, a registry blip) still converges.
	if markerData, err := os.ReadFile(upgradeMarkerPath); err == nil {
		m := parseUpgradeMarker(markerData)
		if m.CurrentSHA == gitShort && sameUpgradeTarget(m.TargetSHA, targetSHA) {
			if m.Attempts >= selfUpgradeMaxAttempts {
				// Terminal: report it LOUDLY and tell the hub, so the UI stops
				// claiming "Upgrading" forever and a human sees the real cause.
				w.logger.Error("self-upgrade FAILED: giving up after repeated attempts (image never changed)",
					"target", targetSHA,
					"current", gitShort,
					"attempts", m.Attempts,
					"max_attempts", selfUpgradeMaxAttempts,
					"last_error", m.LastError,
					"hint", "the spoke must be able to get/patch its own Deployment; check the hive-self-upgrade Role/RoleBinding in this namespace",
				)
				hub.ReportUpgradeFailure(w.hubURL, w.cfg.HiveID, targetSHA, gitShort,
					upgradeFailureSummary(m.Attempts, m.LastError), w.logger)
				return
			}
			// Exponential backoff between attempts so a hard failure does not
			// spin every heartbeat while a recoverable one still retries.
			backoff := selfUpgradeBaseBackoff << (m.Attempts - 1)
			if backoff > selfUpgradeMaxBackoff {
				backoff = selfUpgradeMaxBackoff
			}
			if since := time.Since(m.RequestedAt); since < backoff {
				w.logger.Warn("self-upgrade retry deferred: backing off after a failed attempt",
					"target", targetSHA,
					"current", gitShort,
					"attempts", m.Attempts,
					"retry_in", (backoff - since).Round(time.Second),
					"last_error", m.LastError,
				)
				return
			}
			w.logger.Warn("self-upgrade retrying after a failed attempt (image unchanged)",
				"target", targetSHA,
				"current", gitShort,
				"attempt", m.Attempts+1,
				"max_attempts", selfUpgradeMaxAttempts,
				"last_error", m.LastError,
			)
			attemptCount = m.Attempts
		} else {
			// Different SHA or a different target — the old marker is stale.
			if err := os.Remove(upgradeMarkerPath); err != nil && !os.IsNotExist(err) {
				w.logger.Warn("failed to clear stale upgrade marker", "path", upgradeMarkerPath, "error", err)
			}
		}
	}

	// Minimum uptime before allowing self-upgrade to avoid restart loops.
	const minUptimeBeforeUpgrade = 5 * time.Minute
	uptime := time.Since(w.startTime)
	if uptime < minUptimeBeforeUpgrade {
		w.logger.Warn("self-upgrade deferred: minimum uptime not reached",
			"target", targetSHA,
			"current", gitShort,
			"uptime", uptime.Round(time.Second),
			"min_uptime", minUptimeBeforeUpgrade,
		)
		return
	}

	// Record the attempt BEFORE acting: if the process dies mid-upgrade the
	// next boot must still see an incremented count, otherwise a crash loop
	// would retry without ever exhausting the budget.
	writeUpgradeMarker(upgradeMarkerPath, upgradeMarker{
		TargetSHA:   targetSHA,
		CurrentSHA:  gitShort,
		RequestedAt: time.Now().UTC(),
		Attempts:    attemptCount + 1,
	}, w.logger)

	w.logger.Info("self-upgrade triggered: sending upgrading heartbeat then exiting",
		"current", gitShort,
		"latest", targetSHA,
		"uptime", uptime.Round(time.Second),
	)

	hub.SendUpgradingHeartbeat(w.hubURL, w.buildUpgradingHeartbeatPayload, targetSHA, w.logger)

	// A plain rollout restart only advances a deployment tracking a
	// MUTABLE tag. On a SHA-pinned deployment it relaunches the very
	// same image, so the hive reports the old hash and the hub re-sends
	// this upgrade every heartbeat — a restart loop that never lands.
	// UpgradeSelfToSHA patches the image instead when we are pinned.
	needsRestart, err := hub.UpgradeSelfToSHA(w.logger, targetSHA)
	if err != nil {
		w.logger.Warn("pinned-image upgrade failed, falling back to rolling restart",
			"target", targetSHA, "error", err)
		recordUpgradeError(upgradeMarkerPath, err, w.logger)
		needsRestart = true
	}
	if needsRestart {
		if err := hub.RolloutRestartSelf(w.logger); err != nil {
			// This is the wedge. os.Exit here restarts the pod onto the
			// SAME image, so the upgrade silently never lands. It is an
			// ERROR, not a Warn, and the cause (typically a 403 because
			// the spoke lacks patch on its own Deployment) must be both
			// persisted for the next attempt and reported to the hub so
			// the UI stops showing a permanent "Upgrading".
			w.logger.Error("self-upgrade FAILED: could not patch own Deployment, restarting onto the same image",
				"target", targetSHA,
				"current", gitShort,
				"error", err,
				"hint", "grant get/patch on deployments/hive in this namespace (hive-self-upgrade Role/RoleBinding)",
			)
			recordUpgradeError(upgradeMarkerPath, err, w.logger)
			hub.ReportUpgradeFailure(w.hubURL, w.cfg.HiveID, targetSHA, gitShort, err.Error(), w.logger)
			// Exit NON-ZERO. Exiting 0 on a failed upgrade told Kubernetes
			// the process had completed successfully, so the restart looked
			// routine and nothing — not the pod's exit code, not an event,
			// not a probe — recorded that an upgrade had just failed. A
			// non-zero code makes the failure visible in the pod's
			// lastState.terminated and in `kubectl describe`.
			os.Exit(selfUpgradeFailureExitCode)
		}
	}
	// Rolling restart initiated — K8s will start a new pod and
	// send SIGTERM to this one once the replacement is Ready.
	// Block here so the process stays alive until terminated.
	w.logger.Info("waiting for SIGTERM after rolling restart")
	<-w.ctx.Done()

}

func (w *spokeWire) buildUpgradingHeartbeatPayload() *hub.HeartbeatPayload {
	if !w.cfg.Hub.Enabled {
		return nil
	}
	statuses := w.agentMgr.AllStatuses()
	govState := w.gov.GetState()
	currentMode := strings.ToLower(string(govState.Mode))
	agents := make([]hub.AgentSummary, 0, len(statuses))
	for name, proc := range statuses {
		mode := ""
		if ac, ok := w.cfg.Agents[name]; (ok && ac.OnDemand) || w.onDemandFromPack[name] {
			mode = "on_demand"
		}
		agents = append(agents, hub.NewAgentSummary(name, string(proc.State), mode,
			hub.AgentActivityFor(w.agentMgr, w.cfg, govState, currentMode, name, proc, w.onDemandFromPack)))
	}
	acmmLvl := 0
	if w.cfg.ACMMLevel != nil {
		acmmLvl = *w.cfg.ACMMLevel
	}
	providerLimitReason, providerLimitRebuffs, providerLimitHiveWide, providerLimitAgents := hub.ProviderLimitHeartbeatFields(agents, dashboard.InferenceBudgetExceeded)
	lastWriteKickAt, kickDisposition, kickSkipReason, notWritableQueued :=
		outputFreshnessHeartbeatFields(acmmLvl, govState, agents)
	return &hub.HeartbeatPayload{
		HiveID: w.cfg.HiveID,
		Org:    w.cfg.Project.Org,
		// Project identity rides even this minimal beat. The hub
		// rebuilds the registry entry from each payload VERBATIM
		// (no carry-forward for these fields), and this beat is
		// the LAST one the hub holds for the whole restart window
		// that follows — omitting repos/primary_repo here blanked
		// the entry (org set, primaryRepo "", repos []) until the
		// new process's first successful collect, breaking the
		// public-directory row (no repo link) and rendering the
		// hive name as "org/". Both values are plain config reads,
		// exactly as cheap as Org above.
		Repos:                   w.cfg.Project.Repos,
		PrimaryRepo:             w.cfg.Project.PrimaryRepo,
		ACMMLevel:               acmmLvl,
		Agents:                  agents,
		GitHash:                 gitShort,
		ClusterID:               w.cfg.Hub.ClusterID,
		HiveType:                w.cfg.Hub.HiveType,
		IsPublic:                w.cfg.Hub.IsPublic,
		Version:                 version,
		RepoTargetMisconfigured: w.repoTargetMisconfigured(),
		RepoTargetIssue:         w.repoTargetIssueMessage(),
		ProviderLimitReason:     providerLimitReason,
		ProviderLimitRebuffs:    providerLimitRebuffs,
		ProviderLimitHiveWide:   providerLimitHiveWide,
		ProviderLimitAgents:     providerLimitAgents,
		LastWriteCapableKickAt:  lastWriteKickAt,
		LastKickDisposition:     kickDisposition,
		LastKickSkipReason:      kickSkipReason,
		NotWritableQueued:       notWritableQueued,
		// Remediation-hint detectors (#5577): all three are
		// cheap in-memory reads, so even this minimal upgrading
		// beat carries them — the pod is about to restart, and
		// carrying the last real measurement across the roll keeps
		// a live wedge visible instead of blanking it.
		AgentErrorStreaks: w.tokenCollector.AgentErrorStreaks(),
		ConsentWedged:     w.agentMgr.ConsentWedgedAgents(),
		NoCadenceAgents:   w.gov.NoCadenceAgents(),
	}

}

package github

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// AppRuntimeIdentity is the immutable, non-secret identity of one live GitHub
// App writer. It is safe to expose in status and readiness receipts.
type AppRuntimeIdentity struct {
	AppID             int64             `json:"app_id"`
	InstallationID    int64             `json:"installation_id"`
	InstallationOwner string            `json:"installation_owner"`
	BotID             int64             `json:"bot_id"`
	BotLogin          string            `json:"bot_login"`
	BotType           string            `json:"bot_type"`
	Repository        string            `json:"repository"`
	RepositoryID      int64             `json:"repository_id"`
	Permissions       map[string]string `json:"permissions"`
	PermissionDigest  string            `json:"permission_digest"`
	BindingDigest     string            `json:"binding_digest"`
}

// ResolveAppRuntimeIdentity proves the exact installation, repository, bot,
// and granted permission set without mutating GitHub.
func (c *Client) ResolveAppRuntimeIdentity(ctx context.Context, repository string) (AppRuntimeIdentity, error) {
	if c == nil || c.client == nil || c.appAuth == nil {
		return AppRuntimeIdentity{}, fmt.Errorf("GitHub App client is required")
	}
	owner, name, err := splitFullRepository(repository)
	if err != nil {
		return AppRuntimeIdentity{}, err
	}
	installation, err := c.appAuth.VerifyInstallation(ctx)
	if err != nil {
		return AppRuntimeIdentity{}, err
	}
	if installation.AppID != c.appAuth.AppID() || installation.InstallationID != c.appAuth.InstallationID() {
		return AppRuntimeIdentity{}, fmt.Errorf("GitHub App installation identity does not match the configured App/installation")
	}
	repo, _, err := c.client.Repositories.Get(ctx, owner, name)
	if err != nil {
		return AppRuntimeIdentity{}, fmt.Errorf("verify App access to %s: %w", repository, err)
	}
	if repo.GetID() <= 0 || !strings.EqualFold(strings.TrimSpace(repo.GetFullName()), repository) {
		return AppRuntimeIdentity{}, fmt.Errorf("GitHub App repository identity does not match %s", repository)
	}
	if !strings.EqualFold(strings.TrimSpace(installation.Account), owner) {
		return AppRuntimeIdentity{}, fmt.Errorf("GitHub App installation owner %q does not match repository owner %q", installation.Account, owner)
	}
	botLogin := strings.TrimSpace(c.AppBotLogin())
	if botLogin == "" && strings.TrimSpace(installation.AppSlug) != "" {
		botLogin = strings.TrimSpace(installation.AppSlug) + "[bot]"
	}
	if botLogin == "" {
		return AppRuntimeIdentity{}, fmt.Errorf("GitHub App bot login is unavailable")
	}
	bot, _, err := c.client.Users.Get(ctx, botLogin)
	if err != nil {
		return AppRuntimeIdentity{}, fmt.Errorf("resolve GitHub App bot %q: %w", botLogin, err)
	}
	if bot.GetID() <= 0 || !strings.EqualFold(strings.TrimSpace(bot.GetLogin()), botLogin) || !strings.EqualFold(strings.TrimSpace(bot.GetType()), "Bot") {
		return AppRuntimeIdentity{}, fmt.Errorf("GitHub App bot identity is invalid: login=%q id=%d type=%q", bot.GetLogin(), bot.GetID(), bot.GetType())
	}
	permissions := make(map[string]string, len(installation.Permissions))
	for name, granted := range installation.Permissions {
		if normalized := normalizeAppPermission(granted); normalized != "" {
			permissions[name] = normalized
		}
	}
	// Compatibility with custom forge clients and older test fixtures that do
	// not yet expose the complete permission map. Production GitHub App
	// verification always supplies InstallationInfo.Permissions.
	if len(permissions) == 0 {
		permissions = map[string]string{
			"actions":       normalizeAppPermission(installation.ActionsPerm),
			"workflows":     normalizeAppPermission(installation.WorkflowsPerm),
			"checks":        normalizeAppPermission(installation.ChecksPerm),
			"statuses":      normalizeAppPermission(installation.StatusesPerm),
			"contents":      normalizeAppPermission(installation.ContentsPerm),
			"issues":        normalizeAppPermission(installation.IssuesPerm),
			"pull_requests": normalizeAppPermission(installation.PullsPerm),
			"metadata":      normalizeAppPermission(installation.MetadataPerm),
		}
	}
	permissionDigest, err := digestAppRuntimeValue(permissions)
	if err != nil {
		return AppRuntimeIdentity{}, err
	}
	identity := AppRuntimeIdentity{
		AppID: c.appAuth.AppID(), InstallationID: c.appAuth.InstallationID(),
		InstallationOwner: strings.TrimSpace(installation.Account),
		BotID:             bot.GetID(), BotLogin: strings.TrimSpace(bot.GetLogin()), BotType: strings.TrimSpace(bot.GetType()),
		Repository: strings.TrimSpace(repo.GetFullName()), RepositoryID: repo.GetID(),
		Permissions: permissions, PermissionDigest: permissionDigest,
	}
	identity.BindingDigest, err = digestAppRuntimeValue(struct {
		AppID          int64  `json:"app_id"`
		InstallationID int64  `json:"installation_id"`
		BotID          int64  `json:"bot_id"`
		BotLogin       string `json:"bot_login"`
		Repository     string `json:"repository"`
		RepositoryID   int64  `json:"repository_id"`
	}{identity.AppID, identity.InstallationID, identity.BotID, strings.ToLower(identity.BotLogin), strings.ToLower(identity.Repository), identity.RepositoryID})
	if err != nil {
		return AppRuntimeIdentity{}, err
	}
	return identity, nil
}

// RequireCoreHiveLifecyclePermissions rejects an installation before Hive
// performs repository lifecycle writes. The ordinary Hive App deliberately
// does not need Actions or Commit statuses write access: those two optional
// Visual Hive capabilities belong to the dedicated execution App.
func (identity AppRuntimeIdentity) RequireCoreHiveLifecyclePermissions() error {
	for _, permission := range []string{"workflows", "contents", "issues", "pull_requests"} {
		if identity.Permissions[permission] != "write" {
			return fmt.Errorf("core GitHub App permission %s must be write; granted=%q", permission, identity.Permissions[permission])
		}
	}
	for _, permission := range []string{"actions", "checks", "statuses", "metadata"} {
		if granted := identity.Permissions[permission]; granted != "read" && granted != "write" {
			return fmt.Errorf("core GitHub App permission %s must be read or write; granted=%q", permission, granted)
		}
	}
	return nil
}

// RequireVisualHiveExecutionPermissions validates the deliberately narrow
// optional App used by Hive to dispatch Visual Hive workflows and publish the
// provenance-bound setup status. It rejects every Hive lifecycle write grant
// represented in the audited runtime identity so a misconfigured optional App
// cannot silently become a second lifecycle writer.
func (identity AppRuntimeIdentity) RequireVisualHiveExecutionPermissions() error {
	for _, permission := range []string{"actions", "statuses"} {
		if identity.Permissions[permission] != "write" {
			return fmt.Errorf("Visual Hive GitHub App permission %s must be write; granted=%q", permission, identity.Permissions[permission])
		}
	}
	if granted := identity.Permissions["metadata"]; granted != "read" {
		return fmt.Errorf("Visual Hive GitHub App permission metadata must be read; granted=%q", granted)
	}
	for permission, granted := range identity.Permissions {
		switch permission {
		case "actions", "statuses", "metadata":
		default:
			if granted != "" && granted != "none" {
				return fmt.Errorf("Visual Hive GitHub App permission %s must not be granted; granted=%q", permission, granted)
			}
		}
	}
	return nil
}

// RequireVisualHivePermissions retains the legacy single-App contract for
// existing installations. New hosted installs validate the core and optional
// execution Apps independently with the two methods above.
func (identity AppRuntimeIdentity) RequireVisualHivePermissions() error {
	for _, permission := range []string{"actions", "workflows", "statuses", "contents", "issues", "pull_requests"} {
		if identity.Permissions[permission] != "write" {
			return fmt.Errorf("GitHub App permission %s must be write; granted=%q", permission, identity.Permissions[permission])
		}
	}
	for _, permission := range []string{"checks", "metadata"} {
		if granted := identity.Permissions[permission]; granted != "read" && granted != "write" {
			return fmt.Errorf("GitHub App permission %s must be read or write; granted=%q", permission, granted)
		}
	}
	return nil
}

func normalizeAppPermission(value string) string {
	return strings.ToLower(strings.TrimSpace(value))
}

func digestAppRuntimeValue(value any) (string, error) {
	// Maps are serialized with deterministic sorted keys by encoding/json. Copy
	// through an ordered key set first so this remains explicit and reviewable.
	if permissions, ok := value.(map[string]string); ok {
		keys := make([]string, 0, len(permissions))
		for key := range permissions {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		ordered := make([][2]string, 0, len(keys))
		for _, key := range keys {
			ordered = append(ordered, [2]string{key, permissions[key]})
		}
		value = ordered
	}
	data, err := json.Marshal(value)
	if err != nil {
		return "", fmt.Errorf("encode GitHub App runtime identity: %w", err)
	}
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:]), nil
}

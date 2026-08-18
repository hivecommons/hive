package integrated

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"

	hivegithub "github.com/kubestellar/hive/pkg/github"
)

var setupIdentityDigest = regexp.MustCompile(`^[a-f0-9]{64}$`)

func resolveSetupAuthorizationIdentities(ctx context.Context, options SetupOptions) (hivegithub.AuthenticatedUserIdentity, hivegithub.AuthenticatedUserIdentity, hivegithub.AppRuntimeIdentity, error) {
	if options.GitHub == nil || options.GitHub.GoGitHub() == nil {
		return hivegithub.AuthenticatedUserIdentity{}, hivegithub.AuthenticatedUserIdentity{}, hivegithub.AppRuntimeIdentity{}, errors.New("GitHub client is required to resolve setup identities")
	}
	actor, writer, app := options.AuthorizationActor, options.AuthorizationWriter, options.AuthorizationApp
	explicit := actor.ID != 0 || strings.TrimSpace(actor.Login) != "" || writer.ID != 0 || strings.TrimSpace(writer.Login) != "" || app.AppID != 0
	if !explicit {
		human, err := options.GitHub.AuthenticatedNumericUser(ctx)
		if err != nil {
			return hivegithub.AuthenticatedUserIdentity{}, hivegithub.AuthenticatedUserIdentity{}, hivegithub.AppRuntimeIdentity{}, err
		}
		return human, human, hivegithub.AppRuntimeIdentity{}, nil
	}
	if actor.ID <= 0 || strings.TrimSpace(actor.Login) == "" || !strings.EqualFold(strings.TrimSpace(actor.Type), "User") ||
		writer.ID <= 0 || strings.TrimSpace(writer.Login) == "" || strings.TrimSpace(writer.Type) == "" {
		return hivegithub.AuthenticatedUserIdentity{}, hivegithub.AuthenticatedUserIdentity{}, hivegithub.AppRuntimeIdentity{}, errors.New("setup operator/writer identity handoff is incomplete")
	}
	resolvedActor, err := options.GitHub.ResolveHumanNumericUser(ctx, actor.Login)
	if err != nil {
		return hivegithub.AuthenticatedUserIdentity{}, hivegithub.AuthenticatedUserIdentity{}, hivegithub.AppRuntimeIdentity{}, err
	}
	if resolvedActor.ID != actor.ID || !strings.EqualFold(resolvedActor.Login, actor.Login) {
		return hivegithub.AuthenticatedUserIdentity{}, hivegithub.AuthenticatedUserIdentity{}, hivegithub.AppRuntimeIdentity{}, errors.New("setup operator identity handoff does not match GitHub")
	}
	actor = resolvedActor
	if strings.EqualFold(writer.Type, "User") {
		if app.AppID != 0 || app.InstallationID != 0 || writer.ID != actor.ID || !strings.EqualFold(writer.Login, actor.Login) {
			return hivegithub.AuthenticatedUserIdentity{}, hivegithub.AuthenticatedUserIdentity{}, hivegithub.AppRuntimeIdentity{}, errors.New("standalone setup requires the human operator and writer to be identical")
		}
		authenticated, authErr := options.GitHub.AuthenticatedNumericUser(ctx)
		if authErr != nil || authenticated.ID != actor.ID || !strings.EqualFold(authenticated.Login, actor.Login) {
			return hivegithub.AuthenticatedUserIdentity{}, hivegithub.AuthenticatedUserIdentity{}, hivegithub.AppRuntimeIdentity{}, fmt.Errorf("standalone setup credential does not match the operator: %w", authErr)
		}
		return actor, authenticated, hivegithub.AppRuntimeIdentity{}, nil
	}
	if !strings.EqualFold(writer.Type, "Bot") || app.AppID <= 0 || app.InstallationID <= 0 || app.BotID != writer.ID ||
		!strings.EqualFold(app.BotLogin, writer.Login) || !strings.EqualFold(app.BotType, writer.Type) ||
		!strings.EqualFold(app.Repository, options.Repository) || app.RepositoryID <= 0 ||
		!setupIdentityDigest.MatchString(strings.ToLower(strings.TrimSpace(app.PermissionDigest))) ||
		!setupIdentityDigest.MatchString(strings.ToLower(strings.TrimSpace(app.BindingDigest))) {
		return hivegithub.AuthenticatedUserIdentity{}, hivegithub.AuthenticatedUserIdentity{}, hivegithub.AppRuntimeIdentity{}, errors.New("GitHub App setup writer handoff is invalid or bound to a different repository")
	}
	if err := app.RequireCoreHiveLifecyclePermissions(); err != nil {
		return hivegithub.AuthenticatedUserIdentity{}, hivegithub.AuthenticatedUserIdentity{}, hivegithub.AppRuntimeIdentity{}, err
	}
	return actor, writer, app, nil
}

// resolveVisualHiveGitHubIdentity binds the optional execution/status App
// independently from Hive's lifecycle writer. Standalone PAT mode preserves
// the existing single-human credential contract; hosted App mode requires a
// distinct, least-privilege App so opting out of Visual Hive never expands the
// ordinary Hive App's permissions.
func resolveVisualHiveGitHubIdentity(ctx context.Context, options SetupOptions, lifecycleWriter hivegithub.AuthenticatedUserIdentity, lifecycleApp hivegithub.AppRuntimeIdentity) (hivegithub.AuthenticatedUserIdentity, hivegithub.AppRuntimeIdentity, error) {
	if !options.VisualHive {
		return hivegithub.AuthenticatedUserIdentity{}, hivegithub.AppRuntimeIdentity{}, nil
	}
	if lifecycleApp.AppID == 0 {
		if options.VisualHiveGitHub != nil || options.VisualHiveWriter.ID != 0 || options.VisualHiveApp.AppID != 0 {
			return hivegithub.AuthenticatedUserIdentity{}, hivegithub.AppRuntimeIdentity{}, errors.New("standalone PAT setup cannot mix an optional App credential with the human lifecycle writer")
		}
		return lifecycleWriter, hivegithub.AppRuntimeIdentity{}, nil
	}
	client := options.VisualHiveGitHub
	writer, app := options.VisualHiveWriter, options.VisualHiveApp
	if client == nil || client.GoGitHub() == nil || writer.ID <= 0 || strings.TrimSpace(writer.Login) == "" || !strings.EqualFold(writer.Type, "Bot") {
		return hivegithub.AuthenticatedUserIdentity{}, hivegithub.AppRuntimeIdentity{}, errors.New("hosted Visual Hive setup requires the dedicated GitHub App runtime")
	}
	if app.AppID <= 0 || app.InstallationID <= 0 || app.AppID == lifecycleApp.AppID ||
		app.BotID != writer.ID || !strings.EqualFold(app.BotLogin, writer.Login) || !strings.EqualFold(app.BotType, writer.Type) ||
		!strings.EqualFold(app.Repository, options.Repository) || app.RepositoryID <= 0 ||
		!setupIdentityDigest.MatchString(strings.ToLower(strings.TrimSpace(app.PermissionDigest))) ||
		!setupIdentityDigest.MatchString(strings.ToLower(strings.TrimSpace(app.BindingDigest))) {
		return hivegithub.AuthenticatedUserIdentity{}, hivegithub.AppRuntimeIdentity{}, errors.New("Visual Hive GitHub App handoff is invalid, not separate, or bound to a different repository")
	}
	if err := app.RequireVisualHiveExecutionPermissions(); err != nil {
		return hivegithub.AuthenticatedUserIdentity{}, hivegithub.AppRuntimeIdentity{}, err
	}
	if err := client.VerifyAppWriterBinding(writer, app.BindingDigest); err != nil {
		return hivegithub.AuthenticatedUserIdentity{}, hivegithub.AppRuntimeIdentity{}, fmt.Errorf("verify Visual Hive GitHub App writer: %w", err)
	}
	return writer, app, nil
}

func setupAuthorizationWriter(config Config) (hivegithub.AuthenticatedUserIdentity, bool) {
	if config.SetupAuthorizationWriterID > 0 && strings.TrimSpace(config.SetupAuthorizationWriterLogin) != "" && strings.TrimSpace(config.SetupAuthorizationWriterType) != "" {
		return hivegithub.AuthenticatedUserIdentity{ID: config.SetupAuthorizationWriterID, Login: config.SetupAuthorizationWriterLogin, Type: config.SetupAuthorizationWriterType}, true
	}
	// Legacy PAT contracts bound the human actor directly to the status writer.
	// App-backed mutations never use this fallback because App bindings carry
	// explicit App/installation fields and require managed upgrade/rebind.
	if config.SetupAuthorizationAppID == 0 && config.SetupAuthorizationInstallationID == 0 && config.SetupAuthorizationActorID > 0 {
		return hivegithub.AuthenticatedUserIdentity{ID: config.SetupAuthorizationActorID, Login: config.SetupAuthorizationActorLogin, Type: "User"}, true
	}
	return hivegithub.AuthenticatedUserIdentity{}, false
}

func effectiveSetupAuthorizationWriter(config Config) hivegithub.AuthenticatedUserIdentity {
	writer, _ := setupAuthorizationWriter(config)
	return writer
}

func visualHiveGitHubWriter(config Config) (hivegithub.AuthenticatedUserIdentity, bool) {
	if config.VisualHiveGitHubWriterID > 0 && strings.TrimSpace(config.VisualHiveGitHubWriterLogin) != "" && strings.TrimSpace(config.VisualHiveGitHubWriterType) != "" {
		return hivegithub.AuthenticatedUserIdentity{ID: config.VisualHiveGitHubWriterID, Login: config.VisualHiveGitHubWriterLogin, Type: config.VisualHiveGitHubWriterType}, true
	}
	// Legacy single-App and standalone PAT contracts used the lifecycle writer
	// for both responsibilities. Retain that exact historical binding so a
	// legacy installation can complete its supported cleanup lifecycle after an
	// image upgrade. New hosted setup and production dispatch still require the
	// explicit, separate Visual Hive App binding above; this fallback cannot
	// silently activate or rebind a legacy controller.
	if config.VisualHiveGitHubAppID == 0 && config.VisualHiveGitHubInstallationID == 0 {
		return setupAuthorizationWriter(config)
	}
	return hivegithub.AuthenticatedUserIdentity{}, false
}

func effectiveVisualHiveGitHubWriter(config Config) hivegithub.AuthenticatedUserIdentity {
	writer, _ := visualHiveGitHubWriter(config)
	return writer
}

// validateVisualHiveGitHubTransition prevents an installed dedicated App from
// being silently replaced. Permission changes on the same structural App
// binding are allowed so Hive can publish a reviewed managed upgrade with the
// new permission digest. Legacy contracts with no dedicated App binding may
// acquire one through that same managed setup path.
func validateVisualHiveGitHubTransition(prior Config, writer hivegithub.AuthenticatedUserIdentity, app hivegithub.AppRuntimeIdentity) error {
	if prior.VisualHiveGitHubAppID <= 0 {
		return nil
	}
	priorWriter, ok := visualHiveGitHubWriter(prior)
	if !ok || writer.ID <= 0 || app.AppID <= 0 ||
		priorWriter.ID != writer.ID || !strings.EqualFold(priorWriter.Login, writer.Login) ||
		!strings.EqualFold(priorWriter.Type, writer.Type) ||
		prior.VisualHiveGitHubAppID != app.AppID ||
		prior.VisualHiveGitHubInstallationID != app.InstallationID ||
		!strings.EqualFold(prior.VisualHiveGitHubAppBindingDigest, app.BindingDigest) {
		return errors.New("managed setup Visual Hive App/installation identity changed; use managed uninstall/rebind")
	}
	return nil
}

func resolveManagedOperatorIdentity(ctx context.Context, client *hivegithub.Client, config Config, explicit hivegithub.AuthenticatedUserIdentity, operation string) (hivegithub.AuthenticatedUserIdentity, error) {
	if explicit.ID == 0 && strings.TrimSpace(explicit.Login) == "" {
		return resolveOperatorIdentity(ctx, client, nil, config.Repository, operation)
	}
	if explicit.ID <= 0 || strings.TrimSpace(explicit.Login) == "" || !strings.EqualFold(explicit.Type, "User") ||
		explicit.ID != config.SetupAuthorizationActorID || !strings.EqualFold(explicit.Login, config.SetupAuthorizationActorLogin) {
		return hivegithub.AuthenticatedUserIdentity{}, errors.New("managed lifecycle operator does not match the installed human setup authorizer")
	}
	writer, ok := setupAuthorizationWriter(config)
	if !ok {
		return hivegithub.AuthenticatedUserIdentity{}, errors.New("managed lifecycle requires an exact installed writer binding")
	}
	switch {
	case strings.EqualFold(writer.Type, "User"):
		if config.SetupAuthorizationAppID != 0 || config.SetupAuthorizationInstallationID != 0 ||
			strings.TrimSpace(config.SetupAuthorizationAppBindingDigest) != "" || writer.ID != explicit.ID ||
			!strings.EqualFold(writer.Login, explicit.Login) {
			return hivegithub.AuthenticatedUserIdentity{}, errors.New("standalone managed lifecycle requires the human operator and PAT writer to be identical")
		}
		authenticated, authErr := client.AuthenticatedNumericUser(ctx)
		if authErr != nil {
			return hivegithub.AuthenticatedUserIdentity{}, fmt.Errorf("authenticate standalone managed lifecycle writer: %w", authErr)
		}
		if authenticated.ID != writer.ID || !strings.EqualFold(authenticated.Login, writer.Login) ||
			!strings.EqualFold(authenticated.Type, "User") {
			return hivegithub.AuthenticatedUserIdentity{}, errors.New("standalone managed lifecycle credential does not match the installed human writer")
		}
	case strings.EqualFold(writer.Type, "Bot"):
		if config.SetupAuthorizationAppID <= 0 || config.SetupAuthorizationInstallationID <= 0 ||
			!setupIdentityDigest.MatchString(strings.ToLower(strings.TrimSpace(config.SetupAuthorizationAppBindingDigest))) {
			return hivegithub.AuthenticatedUserIdentity{}, errors.New("App-backed managed lifecycle requires an exact installed App writer binding")
		}
		if err := client.VerifyAppWriterBinding(writer, config.SetupAuthorizationAppBindingDigest); err != nil {
			return hivegithub.AuthenticatedUserIdentity{}, err
		}
	default:
		return hivegithub.AuthenticatedUserIdentity{}, errors.New("managed lifecycle installed writer type is neither User nor Bot")
	}
	resolved, err := client.ResolveHumanNumericUser(ctx, explicit.Login)
	if err != nil || resolved.ID != explicit.ID {
		return hivegithub.AuthenticatedUserIdentity{}, fmt.Errorf("resolve managed lifecycle operator: %w", err)
	}
	return resolved, nil
}

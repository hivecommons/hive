package integrated

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"

	hivegithub "github.com/kubestellar/hive/v2/pkg/github"
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
	if err := app.RequireVisualHivePermissions(); err != nil {
		return hivegithub.AuthenticatedUserIdentity{}, hivegithub.AuthenticatedUserIdentity{}, hivegithub.AppRuntimeIdentity{}, err
	}
	return actor, writer, app, nil
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

func resolveManagedOperatorIdentity(ctx context.Context, client *hivegithub.Client, config Config, explicit hivegithub.AuthenticatedUserIdentity, operation string) (hivegithub.AuthenticatedUserIdentity, error) {
	if explicit.ID == 0 && strings.TrimSpace(explicit.Login) == "" {
		return resolveOperatorIdentity(ctx, client, nil, config.Repository, operation)
	}
	if explicit.ID <= 0 || strings.TrimSpace(explicit.Login) == "" || !strings.EqualFold(explicit.Type, "User") ||
		explicit.ID != config.SetupAuthorizationActorID || !strings.EqualFold(explicit.Login, config.SetupAuthorizationActorLogin) {
		return hivegithub.AuthenticatedUserIdentity{}, errors.New("managed lifecycle operator does not match the installed human setup authorizer")
	}
	writer, ok := setupAuthorizationWriter(config)
	if !ok || !strings.EqualFold(writer.Type, "Bot") || config.SetupAuthorizationAppID <= 0 || config.SetupAuthorizationInstallationID <= 0 ||
		!setupIdentityDigest.MatchString(strings.ToLower(strings.TrimSpace(config.SetupAuthorizationAppBindingDigest))) {
		return hivegithub.AuthenticatedUserIdentity{}, errors.New("App-backed managed lifecycle requires an exact installed App writer binding")
	}
	if err := client.VerifyAppWriterBinding(writer, config.SetupAuthorizationAppBindingDigest); err != nil {
		return hivegithub.AuthenticatedUserIdentity{}, err
	}
	resolved, err := client.ResolveHumanNumericUser(ctx, explicit.Login)
	if err != nil || resolved.ID != explicit.ID {
		return hivegithub.AuthenticatedUserIdentity{}, fmt.Errorf("resolve managed lifecycle operator: %w", err)
	}
	return resolved, nil
}

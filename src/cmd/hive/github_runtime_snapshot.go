package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/kubestellar/hive/pkg/config"
	hivegithub "github.com/kubestellar/hive/pkg/github"
)

var (
	dashboardLiveGitHubRuntime           atomic.Pointer[liveGitHubRuntimeStore]
	dashboardLiveVisualHiveGitHubRuntime atomic.Pointer[liveGitHubRuntimeStore]
)

var resolveLiveGitHubAppIdentity = func(ctx context.Context, client *hivegithub.Client, repository string) (hivegithub.AppRuntimeIdentity, error) {
	return client.ResolveAppRuntimeIdentity(ctx, repository)
}

type liveGitHubRuntimeSnapshot struct {
	Client        *hivegithub.Client
	Token         func(context.Context) (string, error)
	Mode          string
	Repository    string
	RepositoryID  int64
	Writer        hivegithub.AuthenticatedUserIdentity
	App           hivegithub.AppRuntimeIdentity
	BindingDigest string
	Revision      uint64
	Brokered      bool
	ExpiresAt     time.Time
}

type liveGitHubRuntimeStore struct {
	mu       sync.RWMutex
	current  liveGitHubRuntimeSnapshot
	present  bool
	revision uint64
}

func (store *liveGitHubRuntimeStore) Publish(snapshot liveGitHubRuntimeSnapshot) (liveGitHubRuntimeSnapshot, error) {
	if store == nil {
		return liveGitHubRuntimeSnapshot{}, errors.New("live GitHub runtime store is unavailable")
	}
	if snapshot.Client == nil || snapshot.Token == nil || strings.TrimSpace(snapshot.Repository) == "" || snapshot.RepositoryID <= 0 {
		return liveGitHubRuntimeSnapshot{}, errors.New("live GitHub runtime snapshot is incomplete")
	}
	if snapshot.Writer.ID <= 0 || strings.TrimSpace(snapshot.Writer.Login) == "" || strings.TrimSpace(snapshot.Writer.Type) == "" {
		return liveGitHubRuntimeSnapshot{}, errors.New("live GitHub writer identity is incomplete")
	}
	snapshot.Mode = strings.ToLower(strings.TrimSpace(snapshot.Mode))
	if snapshot.Mode != "app" && snapshot.Mode != "pat" {
		return liveGitHubRuntimeSnapshot{}, fmt.Errorf("unsupported live GitHub runtime mode %q", snapshot.Mode)
	}
	if snapshot.Mode == "app" {
		if snapshot.App.AppID <= 0 || snapshot.App.InstallationID <= 0 || snapshot.App.BotID != snapshot.Writer.ID ||
			!strings.EqualFold(snapshot.App.BotLogin, snapshot.Writer.Login) || !strings.EqualFold(snapshot.Writer.Type, "Bot") {
			return liveGitHubRuntimeSnapshot{}, errors.New("live GitHub App writer binding is incomplete")
		}
	}
	if snapshot.Brokered && (snapshot.Mode != "app" || snapshot.ExpiresAt.IsZero() || !snapshot.ExpiresAt.After(time.Now())) {
		return liveGitHubRuntimeSnapshot{}, errors.New("brokered live GitHub runtime lease is expired or invalid")
	}
	digest, err := liveGitHubRuntimeBindingDigest(snapshot)
	if err != nil {
		return liveGitHubRuntimeSnapshot{}, err
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	// Accepted configuration reloads republish the live runtime even when no
	// GitHub identity or credential changed. Treat that as a true no-op: a new
	// revision causes the Visual Hive runtime manager to rotate its controller,
	// which is unnecessary work and briefly gives up the ownership lease. A
	// rebuilt App/PAT client still advances the revision, so real credential
	// rotation is observed without comparing or hashing secret token sources.
	if store.present && store.current.Client == snapshot.Client &&
		store.current.BindingDigest == digest &&
		store.current.App.PermissionDigest == snapshot.App.PermissionDigest {
		// A brokered installation token rotates behind the same dynamic client.
		// Extend only the in-memory lease metadata without advancing the
		// structural revision: the controller keeps the same client and owner,
		// while Current still fails closed at the renewed expiry.
		if store.current.Brokered && snapshot.Brokered && snapshot.ExpiresAt.After(store.current.ExpiresAt) {
			store.current.ExpiresAt = snapshot.ExpiresAt
			store.current.Token = snapshot.Token
		}
		return cloneLiveGitHubRuntime(store.current), nil
	}
	store.revision++
	snapshot.BindingDigest = digest
	snapshot.Revision = store.revision
	// The caller retains ownership of its input. Keep the store independent of
	// later map mutation just as Current keeps its caller independent of us.
	store.current = cloneLiveGitHubRuntime(snapshot)
	store.present = true
	return cloneLiveGitHubRuntime(store.current), nil
}

func (store *liveGitHubRuntimeStore) Current() (liveGitHubRuntimeSnapshot, bool) {
	if store == nil {
		return liveGitHubRuntimeSnapshot{}, false
	}
	store.mu.RLock()
	defer store.mu.RUnlock()
	if !store.present {
		return liveGitHubRuntimeSnapshot{}, false
	}
	if store.current.Brokered && !store.current.ExpiresAt.After(time.Now()) {
		return liveGitHubRuntimeSnapshot{}, false
	}
	return cloneLiveGitHubRuntime(store.current), true
}

func (store *liveGitHubRuntimeStore) Clear() {
	if store == nil {
		return
	}
	store.mu.Lock()
	if !store.present {
		store.mu.Unlock()
		return
	}
	store.current = liveGitHubRuntimeSnapshot{}
	store.present = false
	store.revision++
	store.mu.Unlock()
}

func cloneLiveGitHubRuntime(source liveGitHubRuntimeSnapshot) liveGitHubRuntimeSnapshot {
	result := source
	if source.App.Permissions != nil {
		result.App.Permissions = make(map[string]string, len(source.App.Permissions))
		for permission, granted := range source.App.Permissions {
			result.App.Permissions[permission] = granted
		}
	}
	return result
}

func liveGitHubRuntimeBindingDigest(snapshot liveGitHubRuntimeSnapshot) (string, error) {
	payload := struct {
		Mode             string `json:"mode"`
		Repository       string `json:"repository"`
		RepositoryID     int64  `json:"repository_id"`
		WriterID         int64  `json:"writer_id"`
		WriterLogin      string `json:"writer_login"`
		WriterType       string `json:"writer_type"`
		AppBindingDigest string `json:"app_binding_digest,omitempty"`
		Brokered         bool   `json:"brokered,omitempty"`
	}{
		Mode: strings.ToLower(strings.TrimSpace(snapshot.Mode)), Repository: strings.ToLower(strings.TrimSpace(snapshot.Repository)),
		RepositoryID: snapshot.RepositoryID, WriterID: snapshot.Writer.ID,
		WriterLogin: strings.ToLower(strings.TrimSpace(snapshot.Writer.Login)), WriterType: strings.ToLower(strings.TrimSpace(snapshot.Writer.Type)),
		AppBindingDigest: strings.ToLower(strings.TrimSpace(snapshot.App.BindingDigest)),
		Brokered:         snapshot.Brokered,
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("encode live GitHub runtime binding: %w", err)
	}
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:]), nil
}

func liveGitHubRuntimeForApp(ctx context.Context, client *hivegithub.Client, token func(context.Context) (string, error), repository string) (liveGitHubRuntimeSnapshot, error) {
	identity, err := client.ResolveAppRuntimeIdentity(ctx, repository)
	if err != nil {
		return liveGitHubRuntimeSnapshot{}, err
	}
	writer := hivegithub.AuthenticatedUserIdentity{ID: identity.BotID, Login: identity.BotLogin, Type: identity.BotType}
	if err := client.SetVerifiedAppWriter(writer, identity.BindingDigest); err != nil {
		return liveGitHubRuntimeSnapshot{}, err
	}
	return liveGitHubRuntimeSnapshot{
		Client: client, Token: token, Mode: "app", Repository: identity.Repository, RepositoryID: identity.RepositoryID,
		Writer: writer, App: identity,
	}, nil
}

func liveGitHubRuntimeForPAT(ctx context.Context, client *hivegithub.Client, token func(context.Context) (string, error), repository string) (liveGitHubRuntimeSnapshot, error) {
	if client == nil || token == nil {
		return liveGitHubRuntimeSnapshot{}, errors.New("GitHub PAT runtime is incomplete")
	}
	owner, name, ok := strings.Cut(strings.TrimSpace(repository), "/")
	if !ok || owner == "" || name == "" {
		return liveGitHubRuntimeSnapshot{}, errors.New("GitHub PAT repository binding is invalid")
	}
	writer, err := client.AuthenticatedNumericUser(ctx)
	if err != nil {
		return liveGitHubRuntimeSnapshot{}, err
	}
	metadata, _, err := client.GoGitHub().Repositories.Get(ctx, owner, name)
	if err != nil {
		return liveGitHubRuntimeSnapshot{}, fmt.Errorf("verify PAT repository access: %w", err)
	}
	if metadata.GetID() <= 0 || !strings.EqualFold(metadata.GetFullName(), repository) {
		return liveGitHubRuntimeSnapshot{}, errors.New("GitHub PAT repository identity mismatch")
	}
	return liveGitHubRuntimeSnapshot{Client: client, Token: token, Mode: "pat", Repository: metadata.GetFullName(), RepositoryID: metadata.GetID(), Writer: writer}, nil
}

func publishConfiguredGitHubRuntime(parent context.Context, store *liveGitHubRuntimeStore, client *hivegithub.Client, appAuth *hivegithub.AppAuth, token func(context.Context) (string, error), cfg *config.Config) (liveGitHubRuntimeSnapshot, error) {
	if store == nil || cfg == nil {
		return liveGitHubRuntimeSnapshot{}, errors.New("configured live GitHub runtime is unavailable")
	}
	repository := configuredGitHubRuntimeRepository(cfg)
	if repository == "" {
		return liveGitHubRuntimeSnapshot{}, errors.New("configured live GitHub runtime has no exact repository")
	}
	ctx, cancel := context.WithTimeout(parent, 20*time.Second)
	defer cancel()
	var (
		snapshot liveGitHubRuntimeSnapshot
		err      error
	)
	if appAuth != nil {
		snapshot, err = liveGitHubRuntimeForApp(ctx, client, token, repository)
	} else {
		snapshot, err = liveGitHubRuntimeForPAT(ctx, client, token, repository)
	}
	if err != nil {
		return liveGitHubRuntimeSnapshot{}, err
	}
	if appAuth != nil && (snapshot.App.AppID != cfg.GitHub.AppID || snapshot.App.InstallationID != cfg.GitHub.InstallationID) {
		return liveGitHubRuntimeSnapshot{}, fmt.Errorf(
			"live GitHub App identity %d/%d does not match configured identity %d/%d",
			snapshot.App.AppID, snapshot.App.InstallationID, cfg.GitHub.AppID, cfg.GitHub.InstallationID,
		)
	}
	return store.Publish(snapshot)
}

func configuredGitHubRuntimeRepository(cfg *config.Config) string {
	if cfg == nil {
		return ""
	}
	repository := strings.TrimSpace(cfg.Project.PrimaryRepo)
	if repository == "" && len(cfg.Project.Repos) > 0 {
		repository = strings.TrimSpace(cfg.Project.Repos[0])
	}
	if repository != "" && !strings.Contains(repository, "/") {
		if owner := strings.TrimSpace(cfg.Project.Org); owner != "" {
			repository = owner + "/" + repository
		}
	}
	owner, name, ok := strings.Cut(repository, "/")
	if !ok || owner == "" || name == "" || strings.Contains(name, "/") {
		return ""
	}
	return owner + "/" + name
}

func currentDashboardGitHubRuntime() (liveGitHubRuntimeSnapshot, error) {
	store := dashboardLiveGitHubRuntime.Load()
	if store == nil {
		return liveGitHubRuntimeSnapshot{}, errors.New("live GitHub runtime has not been initialized")
	}
	snapshot, ok := store.Current()
	if !ok {
		return liveGitHubRuntimeSnapshot{}, errors.New("live GitHub runtime is not ready")
	}
	return snapshot, nil
}

func currentDashboardVisualHiveGitHubRuntime() (liveGitHubRuntimeSnapshot, error) {
	store := dashboardLiveVisualHiveGitHubRuntime.Load()
	if store == nil {
		return liveGitHubRuntimeSnapshot{}, errors.New("Visual Hive GitHub runtime has not been initialized")
	}
	snapshot, ok := store.Current()
	if !ok {
		return liveGitHubRuntimeSnapshot{}, errors.New("Visual Hive GitHub runtime is not ready")
	}
	return snapshot, nil
}

// refreshLiveGitHubAppRuntime re-reads the installation's granted permissions
// immediately before a preflight or mutation. GitHub permission updates and
// revocations do not require a config change, so a cached boot-time permission
// set is not sufficient authority for a hosted write.
func refreshLiveGitHubAppRuntime(ctx context.Context, snapshot liveGitHubRuntimeSnapshot) (liveGitHubRuntimeSnapshot, error) {
	return refreshLiveGitHubAppRuntimeInStore(ctx, snapshot, dashboardLiveGitHubRuntime.Load(), "live GitHub")
}

func refreshLiveVisualHiveGitHubAppRuntime(ctx context.Context, snapshot liveGitHubRuntimeSnapshot) (liveGitHubRuntimeSnapshot, error) {
	if snapshot.Brokered {
		if !snapshot.ExpiresAt.After(time.Now().Add(time.Minute)) {
			return liveGitHubRuntimeSnapshot{}, errors.New("Visual Hive GitHub App token lease is expired or too close to expiry")
		}
		if err := snapshot.App.RequireVisualHiveExecutionPermissions(); err != nil {
			return liveGitHubRuntimeSnapshot{}, err
		}
		return snapshot, nil
	}
	return refreshLiveGitHubAppRuntimeInStore(ctx, snapshot, dashboardLiveVisualHiveGitHubRuntime.Load(), "Visual Hive GitHub")
}

func refreshLiveGitHubAppRuntimeInStore(ctx context.Context, snapshot liveGitHubRuntimeSnapshot, store *liveGitHubRuntimeStore, label string) (liveGitHubRuntimeSnapshot, error) {
	if snapshot.Mode != "app" {
		return snapshot, nil
	}
	identity, err := resolveLiveGitHubAppIdentity(ctx, snapshot.Client, snapshot.Repository)
	if err != nil {
		return liveGitHubRuntimeSnapshot{}, fmt.Errorf("refresh live GitHub App identity: %w", err)
	}
	if identity.AppID != snapshot.App.AppID || identity.InstallationID != snapshot.App.InstallationID ||
		identity.RepositoryID != snapshot.RepositoryID || !strings.EqualFold(identity.Repository, snapshot.Repository) ||
		identity.BotID != snapshot.Writer.ID || !strings.EqualFold(identity.BotLogin, snapshot.Writer.Login) ||
		!strings.EqualFold(identity.BotType, snapshot.Writer.Type) || identity.BindingDigest != snapshot.App.BindingDigest {
		return liveGitHubRuntimeSnapshot{}, errors.New("live GitHub App structural identity changed; managed rebind is required")
	}
	writer := hivegithub.AuthenticatedUserIdentity{ID: identity.BotID, Login: identity.BotLogin, Type: identity.BotType}
	if err := snapshot.Client.SetVerifiedAppWriter(writer, identity.BindingDigest); err != nil {
		return liveGitHubRuntimeSnapshot{}, err
	}
	snapshot.App = identity
	snapshot.Writer = writer
	snapshot.BindingDigest = ""
	snapshot.Revision = 0
	if store == nil {
		return liveGitHubRuntimeSnapshot{}, fmt.Errorf("%s runtime has not been initialized", label)
	}
	return store.Publish(snapshot)
}

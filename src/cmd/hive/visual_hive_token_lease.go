package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	hivegithub "github.com/kubestellar/hive/pkg/github"
	"github.com/kubestellar/hive/pkg/hub"
)

const visualHiveGitHubAppEnabledEnv = "HIVE_VISUAL_HIVE_GITHUB_APP_ENABLED"

// visualHiveGitHubAppBrokerEnabled is deliberately an explicit per-instance
// opt-in. A Hub may hold the optional App key for selected repositories, but
// ordinary hosted Hives must not create recipient state or make broker calls
// merely because that fleet-level capability exists.
func visualHiveGitHubAppBrokerEnabled() bool {
	return strings.EqualFold(strings.TrimSpace(os.Getenv(visualHiveGitHubAppEnabledEnv)), "true")
}

// visualHiveTokenLeaseRuntime owns only memory-resident token material. Its
// X25519 private key is persisted by pkg/hub on /data, but the GitHub token is
// never written there (or anywhere else).
type visualHiveTokenLeaseRuntime struct {
	mu         sync.RWMutex
	hiveID     string
	repository string
	request    hub.VisualHiveTokenRequest
	store      *liveGitHubRuntimeStore
	logger     *slog.Logger
	now        func() time.Time

	token     string
	expiresAt time.Time
	identity  hivegithub.AppRuntimeIdentity
	client    *hivegithub.Client
	lastError string
}

func newVisualHiveTokenLeaseRuntime(hiveID, repository string, store *liveGitHubRuntimeStore, logger *slog.Logger) (*visualHiveTokenLeaseRuntime, error) {
	request, err := hub.NewVisualHiveTokenRequest(repository, time.Now().UTC())
	if err != nil {
		return nil, err
	}
	return &visualHiveTokenLeaseRuntime{
		hiveID: strings.TrimSpace(hiveID), repository: strings.TrimSpace(repository), request: *request,
		store: store, logger: logger, now: func() time.Time { return time.Now().UTC() },
	}, nil
}

func (runtime *visualHiveTokenLeaseRuntime) Request() *hub.VisualHiveTokenRequest {
	if runtime == nil {
		return nil
	}
	runtime.mu.RLock()
	defer runtime.mu.RUnlock()
	request := runtime.request
	if runtime.identity.AppID > 0 && runtime.expiresAt.After(runtime.now()) {
		request.CurrentAppID = runtime.identity.AppID
		request.CurrentInstallationID = runtime.identity.InstallationID
		request.CurrentBindingDigest = runtime.identity.BindingDigest
		request.CurrentExpiresAt = runtime.expiresAt
	}
	return &request
}

func (runtime *visualHiveTokenLeaseRuntime) Token(context.Context) (string, error) {
	if runtime == nil {
		return "", errors.New("Visual Hive token lease runtime is unavailable")
	}
	runtime.mu.RLock()
	defer runtime.mu.RUnlock()
	if strings.TrimSpace(runtime.token) == "" || !runtime.expiresAt.After(runtime.now().Add(time.Minute)) {
		return "", errors.New("Visual Hive GitHub App token lease is expired")
	}
	return runtime.token, nil
}

func (runtime *visualHiveTokenLeaseRuntime) Apply(lease *hub.VisualHiveTokenLease, brokerError string) error {
	if runtime == nil {
		return errors.New("Visual Hive token lease runtime is unavailable")
	}
	if lease == nil {
		runtime.mu.Lock()
		runtime.lastError = strings.TrimSpace(brokerError)
		expired := !runtime.expiresAt.After(runtime.now().Add(time.Minute))
		if expired {
			runtime.token = ""
		}
		runtime.mu.Unlock()
		if expired && runtime.store != nil {
			runtime.store.Clear()
		}
		if brokerError != "" && expired {
			return fmt.Errorf("Visual Hive GitHub App broker: %s", brokerError)
		}
		return nil
	}
	material, err := hub.OpenVisualHiveTokenLease(runtime.hiveID, runtime.repository, lease, runtime.now())
	if err != nil {
		return err
	}
	if err := material.Identity.RequireVisualHiveExecutionPermissions(); err != nil {
		return err
	}

	runtime.mu.Lock()
	if runtime.identity.AppID > 0 && (runtime.identity.AppID != material.Identity.AppID || runtime.identity.InstallationID != material.Identity.InstallationID ||
		runtime.identity.RepositoryID != material.Identity.RepositoryID || !strings.EqualFold(runtime.identity.Repository, material.Identity.Repository) ||
		runtime.identity.BindingDigest != material.Identity.BindingDigest) {
		runtime.token = ""
		runtime.expiresAt = time.Time{}
		runtime.mu.Unlock()
		if runtime.store != nil {
			runtime.store.Clear()
		}
		return errors.New("Visual Hive GitHub App binding changed; managed uninstall/rebind is required")
	}
	if runtime.client == nil {
		owner, name, ok := strings.Cut(material.Identity.Repository, "/")
		if !ok || owner == "" || name == "" {
			runtime.mu.Unlock()
			return errors.New("Visual Hive GitHub App repository binding is invalid")
		}
		runtime.client = hivegithub.NewClientWithTokenSource(runtime.Token, owner, []string{name}, runtime.logger, "")
	}
	runtime.token = material.Token
	runtime.expiresAt = material.ExpiresAt
	runtime.identity = material.Identity
	runtime.lastError = ""
	client := runtime.client
	runtime.mu.Unlock()

	writer := hivegithub.AuthenticatedUserIdentity{ID: material.Identity.BotID, Login: material.Identity.BotLogin, Type: material.Identity.BotType}
	if err := client.SetVerifiedAppWriter(writer, material.Identity.BindingDigest); err != nil {
		return err
	}
	if runtime.store == nil {
		return errors.New("Visual Hive live GitHub runtime store is unavailable")
	}
	_, err = runtime.store.Publish(liveGitHubRuntimeSnapshot{
		Client: client, Token: runtime.Token, Mode: "app", Repository: material.Identity.Repository, RepositoryID: material.Identity.RepositoryID,
		Writer: writer, App: material.Identity, Brokered: true, ExpiresAt: material.ExpiresAt,
	})
	return err
}

func (runtime *visualHiveTokenLeaseRuntime) LastError() string {
	if runtime == nil {
		return ""
	}
	runtime.mu.RLock()
	defer runtime.mu.RUnlock()
	return runtime.lastError
}

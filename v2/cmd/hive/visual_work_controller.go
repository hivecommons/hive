package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/kubestellar/hive/v2/pkg/config"
	"github.com/kubestellar/hive/v2/pkg/dashboard"
	hivegithub "github.com/kubestellar/hive/v2/pkg/github"
	"github.com/kubestellar/hive/v2/pkg/integrated"
	"github.com/kubestellar/hive/v2/pkg/visualhive"
	visualcontroller "github.com/kubestellar/hive/v2/pkg/visualhive/controller"
)

type dashboardVisualWorkAudit struct{ server *dashboard.Server }

func (sink dashboardVisualWorkAudit) RecordVisualWorkAudit(_ context.Context, event visualcontroller.AuditEvent) error {
	if sink.server == nil {
		return errors.New("normal dashboard audit sink is unavailable")
	}
	detail := fmt.Sprintf("stage=%s source=%s finding=%s detail=%s", event.Stage, event.SourceExternalRef, event.RepositoryFingerprint, event.Detail)
	if event.Decision != nil {
		detail += fmt.Sprintf(" decision=%s request=%s", event.Decision.Code, event.Decision.RequestSHA256)
	}
	if strings.TrimSpace(event.EventID) != "" {
		_, err := sink.server.AuditLogOnce(event.EventID, "governor", "visual_work", detail, "")
		return err
	}
	sink.server.AuditLog("governor", "visual_work", detail, "")
	return nil
}

func loadCurrentVisualWorkContract(normal *config.Config) (integrated.Config, bool, error) {
	if normal == nil {
		return integrated.Config{}, false, errors.New("normal Hive config is required")
	}
	installed, exists, err := loadAuthoritativeVisualWorkContract()
	if err != nil || !exists {
		return integrated.Config{}, exists, err
	}
	if !normalProjectContainsRepository(normal.Project, installed.Repository) {
		return integrated.Config{}, false, fmt.Errorf("installed Visual Hive repository %s is outside normal Hive project scope", installed.Repository)
	}
	return installed, true, nil
}

func loadAuthoritativeVisualWorkContract() (integrated.Config, bool, error) {
	if err := validateHostedIntegratedStateRoot(); err != nil {
		return integrated.Config{}, false, err
	}
	stateDir := strings.TrimSpace(os.Getenv("HIVE_STATE_DIR"))
	exists := stateDir != ""
	var err error
	if exists {
		stateDir, err = filepath.Abs(stateDir)
		if err != nil {
			return integrated.Config{}, false, fmt.Errorf("resolve HIVE_STATE_DIR: %w", err)
		}
		info, statErr := os.Stat(stateDir)
		if statErr != nil || !info.IsDir() {
			if statErr == nil {
				statErr = errors.New("path is not a directory")
			}
			return integrated.Config{}, false, fmt.Errorf("HIVE_STATE_DIR %s is unavailable: %w", stateDir, statErr)
		}
	} else {
		stateDir, exists, err = integrated.CurrentState(integratedStateRoot())
		if err != nil || !exists {
			return integrated.Config{}, exists, err
		}
	}
	storeDir := filepath.Join(stateDir, "integrated")
	if _, statErr := os.Lstat(storeDir); os.IsNotExist(statErr) {
		return integrated.Config{}, false, nil
	} else if statErr != nil {
		return integrated.Config{}, false, fmt.Errorf("inspect authoritative Visual Hive state: %w", statErr)
	}
	store, err := integrated.NewStore(storeDir)
	if err != nil {
		return integrated.Config{}, false, err
	}
	installed, err := store.Load()
	if err != nil {
		if os.IsNotExist(err) {
			entries, readErr := os.ReadDir(filepath.Join(stateDir, "integrated"))
			if readErr == nil && len(entries) == 0 {
				return integrated.Config{}, false, nil
			}
		}
		return integrated.Config{}, false, err
	}
	if !sameSpecialistRuntimePath(installed.StateDir, stateDir) {
		return integrated.Config{}, false, fmt.Errorf("authoritative Visual Hive state identity drifted: selected %s but installed contract names %s", stateDir, installed.StateDir)
	}
	return installed, true, nil
}

func normalProjectContainsRepository(project config.ProjectConfig, repository string) bool {
	repository = strings.ToLower(strings.TrimSpace(repository))
	for _, configured := range project.Repos {
		configured = strings.ToLower(strings.TrimSpace(configured))
		if !strings.Contains(configured, "/") && strings.TrimSpace(project.Org) != "" {
			configured = strings.ToLower(strings.TrimSpace(project.Org)) + "/" + configured
		}
		if configured == repository {
			return true
		}
	}
	return false
}

func importNormalVisualWork(ctx context.Context, source hivegithub.VerifiedVisualHiveArtifact) (visualcontroller.Result, error) {
	normalVisualRuntime := dashboardNormalVisualRuntime.Load()
	if normalVisualRuntime == nil {
		return visualcontroller.Result{}, errors.New("normal Visual Hive work service is not configured")
	}
	return normalVisualRuntime.Import(ctx, source)
}

func visualLifecycleForInstalledContract(installed integrated.Config) (*visualhive.LifecycleStore, error) {
	return visualhive.NewLifecycleStore(filepath.Join(installed.StateDir, "visual-hive"))
}

package integrated

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/kubestellar/hive/v2/pkg/automation"
	hivegithub "github.com/kubestellar/hive/v2/pkg/github"
)

type ManagementOperation string

const (
	OperationUpgrade   ManagementOperation = "upgrade"
	OperationRollback  ManagementOperation = "rollback"
	OperationUninstall ManagementOperation = "uninstall"
)

type ManagementOptions struct {
	Operation     ManagementOperation
	StateDir      string
	VisualHiveRef string
	DeleteState   bool
	GitHub        *hivegithub.Client
}

type ManagementResult struct {
	SchemaVersion string              `json:"schema_version"`
	Operation     ManagementOperation `json:"operation"`
	Repository    string              `json:"repository"`
	Branch        string              `json:"branch"`
	CommitSHA     string              `json:"commit_sha"`
	PRNumber      int                 `json:"pr_number"`
	PRURL         string              `json:"pr_url"`
	PreviousRef   string              `json:"previous_ref,omitempty"`
	RequestedRef  string              `json:"requested_ref,omitempty"`
	Idempotent    bool                `json:"idempotent"`
	StateDeleted  bool                `json:"state_deleted"`
}

func RunManagement(ctx context.Context, options ManagementOptions) (ManagementResult, error) {
	result := ManagementResult{SchemaVersion: "hive.management.v1", Operation: options.Operation}
	if options.GitHub == nil || strings.TrimSpace(options.StateDir) == "" {
		return result, fmt.Errorf("GitHub client and persistent state directory are required")
	}
	if options.Operation != OperationUpgrade && options.Operation != OperationRollback && options.Operation != OperationUninstall {
		return result, fmt.Errorf("unsupported management operation %q", options.Operation)
	}
	store, err := NewStore(filepath.Join(options.StateDir, "integrated"))
	if err != nil {
		return result, err
	}
	config, err := store.Load()
	if err != nil {
		return result, err
	}
	result.Repository, result.PreviousRef = config.Repository, config.VisualHiveRef
	policy := automation.Policy{ACMMLevel: config.ACMMLevel, Mode: automationMode(config.Automation), AllowedRepositories: []string{config.Repository}}
	if options.Operation == OperationUpgrade || options.Operation == OperationRollback {
		requested := strings.ToLower(strings.TrimSpace(options.VisualHiveRef))
		if requested == "" && options.Operation == OperationRollback {
			requested = strings.ToLower(strings.TrimSpace(config.PreviousVersion))
		}
		if !regexp.MustCompile(`^[a-f0-9]{40}$`).MatchString(requested) {
			return result, fmt.Errorf("%s requires an immutable 40-character Visual Hive commit SHA", options.Operation)
		}
		result.RequestedRef, options.VisualHiveRef = requested, requested
		if requested == config.VisualHiveRef {
			result.Idempotent = true
			return result, nil
		}
	}
	branch := "hive/" + string(options.Operation)
	if err := authorizeSetup(store, policy, config.Repository, automation.ActionSetupBranch); err != nil {
		return result, err
	}
	defaultBranch, err := ensureCheckout(ctx, config.Repository, config.CheckoutDir)
	if err != nil {
		return result, err
	}
	if _, err := git(ctx, config.CheckoutDir, "switch", "-C", branch, "origin/"+defaultBranch); err != nil {
		return result, err
	}
	managed := []string{".hive/integrated.json", ".github/workflows/hive-visual-hive.yml", "docs/hive-quickstart.md"}
	title := ""
	marker := fmt.Sprintf("<!-- hive-%s: %s -->", options.Operation, strings.ToLower(config.Repository))
	candidate := config
	switch options.Operation {
	case OperationUpgrade, OperationRollback:
		requested := options.VisualHiveRef
		candidate.PreviousVersion, candidate.VisualHiveRef, candidate.UpdatedAt = config.VisualHiveRef, requested, time.Now().UTC()
		inspection, inspectErr := InspectCheckout(config.CheckoutDir, defaultBranch)
		if inspectErr != nil {
			return result, inspectErr
		}
		if err := writeManagedFiles(config.CheckoutDir, candidate, inspection); err != nil {
			return result, err
		}
		title = fmt.Sprintf("%s Visual Hive to %s", strings.Title(string(options.Operation)), requested[:12])
	case OperationUninstall:
		candidate.Paused = true
		for _, relative := range managed {
			if err := os.Remove(filepath.Join(config.CheckoutDir, filepath.FromSlash(relative))); err != nil && !os.IsNotExist(err) {
				return result, err
			}
		}
		title = "Uninstall Hive production automation"
	}
	if err := authorizeSetup(store, policy, config.Repository, automation.ActionSetupCommit); err != nil {
		return result, err
	}
	addArgs := append([]string{"add", "-A", "--"}, managed...)
	if _, err := git(ctx, config.CheckoutDir, addArgs...); err != nil {
		return result, err
	}
	changed, err := git(ctx, config.CheckoutDir, "diff", "--cached", "--name-only")
	if err != nil {
		return result, err
	}
	result.Idempotent = strings.TrimSpace(changed) == ""
	if !result.Idempotent {
		message := "chore: " + string(options.Operation) + " Hive integration"
		if _, err := git(ctx, config.CheckoutDir, "-c", "user.name=Hive Setup", "-c", "user.email=hive-setup@users.noreply.github.com", "commit", "-m", message); err != nil {
			return result, err
		}
	}
	sha, err := git(ctx, config.CheckoutDir, "rev-parse", "HEAD")
	if err != nil {
		return result, err
	}
	result.CommitSHA, result.Branch = strings.TrimSpace(sha), branch
	if err := authorizeSetup(store, policy, config.Repository, automation.ActionSetupPush); err != nil {
		return result, err
	}
	if _, err := git(ctx, config.CheckoutDir, "push", "--force-with-lease", "origin", "HEAD:refs/heads/"+branch); err != nil {
		return result, err
	}
	if err := authorizeSetup(store, policy, config.Repository, automation.ActionSetupPR); err != nil {
		return result, err
	}
	body := fmt.Sprintf("%s\n\nHive-managed `%s` operation.\n\n- Previous Visual Hive ref: `%s`\n- Requested Visual Hive ref: `%s`\n\nThis PR is intentionally reviewable and idempotent. Hive remains paused immediately for uninstall; upgrade and rollback take effect after this PR is merged.", marker, options.Operation, config.VisualHiveRef, result.RequestedRef)
	pull, err := options.GitHub.UpsertRepairPullRequest(ctx, config.Repository, branch, defaultBranch, title, body, marker)
	if err != nil {
		return result, err
	}
	result.PRNumber, result.PRURL = pull.Number, pull.URL
	if options.Operation == OperationUninstall {
		config.Paused = true
		if err := store.Save(config); err != nil {
			return result, err
		}
		store.Audit(AuditEntry{Action: "uninstall", Allowed: true, Repository: config.Repository, Detail: result.PRURL})
		if options.DeleteState {
			if err := deleteManagedState(options.StateDir); err != nil {
				return result, err
			}
			result.StateDeleted = true
		}
	} else {
		if err := store.Save(candidate); err != nil {
			return result, err
		}
		store.Audit(AuditEntry{Action: string(options.Operation), Allowed: true, Repository: config.Repository, Detail: result.PRURL})
	}
	return result, nil
}

func deleteManagedState(stateDir string) error {
	absolute, err := filepath.Abs(stateDir)
	if err != nil {
		return err
	}
	absolute = filepath.Clean(absolute)
	volume := filepath.VolumeName(absolute) + string(os.PathSeparator)
	home, _ := os.UserHomeDir()
	if absolute == volume || absolute == filepath.Clean(home) || len(filepath.SplitList(absolute)) == 0 {
		return fmt.Errorf("refusing to delete unsafe state path %s", absolute)
	}
	marker := filepath.Join(absolute, "integrated", "config.json")
	if info, statErr := os.Stat(marker); statErr != nil || info.IsDir() {
		return fmt.Errorf("refusing to delete %s without a managed Hive config marker", absolute)
	}
	return os.RemoveAll(absolute)
}

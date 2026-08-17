package main

import (
	"bytes"
	"context"
	cryptorand "crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	hivegithub "github.com/kubestellar/hive/v2/pkg/github"
)

const (
	dashboardOperatorHandoffSchema = "hive.dashboard-operator-handoff.v1"
	dashboardOperatorHandoffDir    = "/var/run/hive/operator-handoffs"
	dashboardOperatorHandoffTTL    = 2 * time.Minute
	writeOperatorHandoffCommand    = "_write-dashboard-operator-handoff"
	consumeOperatorHandoffCommand  = "_consume-dashboard-operator-handoff"
	removeOperatorHandoffCommand   = "_remove-dashboard-operator-handoff"
)

var (
	dashboardOperatorRequestIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{7,127}$`)
	dashboardOperatorDigestPattern    = regexp.MustCompile(`^[a-f0-9]{64}$`)
	dashboardOperatorHandoffNow       = func() time.Time { return time.Now().UTC() }
	dashboardOperatorHandoffRoot      = func() string {
		if value := strings.TrimSpace(os.Getenv("HIVE_OPERATOR_HANDOFF_DIR")); value != "" {
			return filepath.Clean(value)
		}
		return filepath.Clean(dashboardOperatorHandoffDir)
	}
	dashboardOperatorHandoffExec = runDashboardOperatorHandoffHelper
	dashboardOperatorHandoffRead = dashboardOperatorHandoffExecOutput
)

type dashboardOperatorHandoff struct {
	SchemaVersion      string                               `json:"schema_version"`
	Repository         string                               `json:"repository"`
	RequestID          string                               `json:"request_id"`
	ExpectedPlanSHA256 string                               `json:"expected_plan_sha256"`
	Actor              hivegithub.AuthenticatedUserIdentity `json:"actor"`
	Writer             hivegithub.AuthenticatedUserIdentity `json:"writer"`
	App                hivegithub.AppRuntimeIdentity        `json:"app,omitempty"`
	IssuedAt           time.Time                            `json:"issued_at"`
	ExpiresAt          time.Time                            `json:"expires_at"`
	Nonce              string                               `json:"nonce"`
	BindingSHA256      string                               `json:"binding_sha256"`
}

func createDashboardOperatorHandoff(ctx context.Context, repository, requestID, expectedPlan string, actor hivegithub.AuthenticatedUserIdentity, runtime liveGitHubRuntimeSnapshot) (string, error) {
	nonce := make([]byte, 24)
	if _, err := cryptorand.Read(nonce); err != nil {
		return "", fmt.Errorf("create dashboard operator nonce: %w", err)
	}
	now := dashboardOperatorHandoffNow()
	handoff := dashboardOperatorHandoff{
		SchemaVersion: dashboardOperatorHandoffSchema, Repository: repository, RequestID: requestID,
		ExpectedPlanSHA256: expectedPlan, Actor: actor, Writer: runtime.Writer, App: runtime.App,
		IssuedAt: now, ExpiresAt: now.Add(dashboardOperatorHandoffTTL), Nonce: hex.EncodeToString(nonce),
	}
	var err error
	handoff.BindingSHA256, err = dashboardOperatorHandoffDigest(handoff)
	if err != nil {
		return "", err
	}
	data, err := json.Marshal(handoff)
	if err != nil {
		return "", fmt.Errorf("encode dashboard operator handoff: %w", err)
	}
	path := filepath.Join(dashboardOperatorHandoffRoot(), "setup-"+handoff.Nonce+".json")
	if err := dashboardOperatorHandoffExec(ctx, writeOperatorHandoffCommand, path, data); err != nil {
		return "", err
	}
	return path, nil
}

func consumeDashboardOperatorHandoff(ctx context.Context, path, repository, requestID, expectedPlan string) (dashboardOperatorHandoff, error) {
	data, err := dashboardOperatorHandoffRead(ctx, consumeOperatorHandoffCommand, path)
	if err != nil {
		return dashboardOperatorHandoff{}, err
	}
	var handoff dashboardOperatorHandoff
	if err := json.Unmarshal(data, &handoff); err != nil {
		return dashboardOperatorHandoff{}, fmt.Errorf("decode dashboard operator handoff: %w", err)
	}
	expectedDigest, err := dashboardOperatorHandoffDigest(handoff)
	if err != nil {
		return dashboardOperatorHandoff{}, err
	}
	now := dashboardOperatorHandoffNow()
	if handoff.SchemaVersion != dashboardOperatorHandoffSchema || handoff.BindingSHA256 != expectedDigest ||
		!strings.EqualFold(handoff.Repository, repository) || handoff.RequestID != requestID || handoff.ExpectedPlanSHA256 != expectedPlan ||
		handoff.Actor.ID <= 0 || !strings.EqualFold(handoff.Actor.Type, "User") || handoff.Writer.ID <= 0 ||
		handoff.IssuedAt.IsZero() || handoff.ExpiresAt.IsZero() || now.Before(handoff.IssuedAt.Add(-30*time.Second)) || !handoff.ExpiresAt.After(now) ||
		!regexp.MustCompile(`^[a-f0-9]{48}$`).MatchString(handoff.Nonce) {
		return dashboardOperatorHandoff{}, errors.New("dashboard operator handoff is expired, malformed, or bound to different setup inputs")
	}
	return handoff, nil
}

func dashboardOperatorHandoffDigest(handoff dashboardOperatorHandoff) (string, error) {
	handoff.BindingSHA256 = ""
	data, err := json.Marshal(handoff)
	if err != nil {
		return "", fmt.Errorf("encode dashboard operator handoff binding: %w", err)
	}
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:]), nil
}

func runDashboardOperatorHandoffHelper(ctx context.Context, operation, path string, input []byte) error {
	_, err := dashboardOperatorHandoffExecOutputWithInput(ctx, operation, path, input)
	return err
}

func dashboardOperatorHandoffExecOutput(ctx context.Context, operation, path string) ([]byte, error) {
	return dashboardOperatorHandoffExecOutputWithInput(ctx, operation, path, nil)
}

func dashboardOperatorHandoffExecOutputWithInput(ctx context.Context, operation, path string, input []byte) ([]byte, error) {
	executable, err := os.Executable()
	if err != nil {
		return nil, err
	}
	args := []string{operation, path}
	var command *exec.Cmd
	if os.Geteuid() == 0 {
		command = exec.CommandContext(ctx, executable, args...)
	} else {
		command = exec.CommandContext(ctx, "su-exec", append([]string{"root", executable}, args...)...)
	}
	command.Stdin = bytes.NewReader(input)
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	if err := command.Run(); err != nil {
		return nil, fmt.Errorf("dashboard operator handoff helper %s failed: %w: %s", operation, err, boundedMCPDiagnostic(stderr.Bytes()))
	}
	return stdout.Bytes(), nil
}

func runDashboardOperatorHandoffChild(operation, path string, stdin io.Reader, stdout io.Writer) int {
	if os.Geteuid() != 0 {
		fmt.Fprintln(os.Stderr, "dashboard operator handoff helper requires root")
		return 1
	}
	root := dashboardOperatorHandoffRoot()
	path = filepath.Clean(path)
	if !pathWithinDashboardOperatorRoot(root, path) {
		fmt.Fprintln(os.Stderr, "dashboard operator handoff path is outside the fixed root")
		return 1
	}
	switch operation {
	case writeOperatorHandoffCommand:
		data, err := io.ReadAll(io.LimitReader(stdin, 64*1024))
		if err != nil || len(data) == 0 {
			fmt.Fprintln(os.Stderr, "dashboard operator handoff payload is unavailable")
			return 1
		}
		if err := os.MkdirAll(root, 0o750); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		group, err := user.LookupGroup("hive-launch")
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		gid, err := strconv.Atoi(group.Gid)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		if err := os.Chown(root, 0, gid); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		if err := os.Chmod(root, 0o750); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		if err := validateDashboardOperatorHandoffRoot(root, gid); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0o640)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		writeErr := error(nil)
		if _, err = file.Write(data); err != nil {
			writeErr = err
		} else if err = file.Sync(); err != nil {
			writeErr = err
		} else if err = file.Chown(0, gid); err != nil {
			writeErr = err
		} else if err = file.Chmod(0o640); err != nil {
			writeErr = err
		}
		if closeErr := file.Close(); writeErr == nil {
			writeErr = closeErr
		}
		if writeErr != nil {
			_ = os.Remove(path)
			fmt.Fprintln(os.Stderr, writeErr)
			return 1
		}
		return 0
	case consumeOperatorHandoffCommand:
		group, groupErr := user.LookupGroup("hive-launch")
		if groupErr != nil {
			fmt.Fprintln(os.Stderr, groupErr)
			return 1
		}
		gid, groupErr := strconv.Atoi(group.Gid)
		if groupErr != nil {
			fmt.Fprintln(os.Stderr, groupErr)
			return 1
		}
		if err := validateDashboardOperatorHandoffRoot(root, gid); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		data, err := readStrictDashboardOperatorHandoff(path, uint32(gid))
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		if err := os.Remove(path); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		if _, err := stdout.Write(data); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		return 0
	case removeOperatorHandoffCommand:
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		return 0
	default:
		fmt.Fprintln(os.Stderr, "unknown dashboard operator handoff helper")
		return 2
	}
}

func readStrictDashboardOperatorHandoff(path string, expectedGID uint32) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0o640 {
		return nil, errors.New("dashboard operator handoff is not one root-owned mode-0640 regular file")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != 0 || stat.Gid != expectedGID {
		return nil, errors.New("dashboard operator handoff is not root:hive-launch owned")
	}
	data, err := os.ReadFile(path)
	if err != nil || len(data) == 0 || len(data) > 64*1024 {
		return nil, errors.New("dashboard operator handoff payload is unavailable or oversized")
	}
	return data, nil
}

func validateDashboardOperatorHandoffRoot(root string, expectedGID int) error {
	info, err := os.Lstat(root)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0o750 {
		return errors.New("dashboard operator handoff root is not one real mode-0750 directory")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != 0 || stat.Gid != uint32(expectedGID) {
		return errors.New("dashboard operator handoff root is not root:hive-launch owned")
	}
	return nil
}

func pathWithinDashboardOperatorRoot(root, path string) bool {
	relative, err := filepath.Rel(filepath.Clean(root), filepath.Clean(path))
	return err == nil && relative != "." && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) && !filepath.IsAbs(relative)
}

package repair

import (
	"context"
	"fmt"
	"os"
	"strings"
)

const (
	transportFetchRef = "refs/hive/transport/fetch"
	transportPushRef  = "refs/hive/transport/push"
)

// runSanitizedGitTransport performs authenticated network Git only from a
// fresh controller-owned bare repository. Target checkout config is never in
// scope while the transport header exists. Fetched objects cross back into the
// target through a credentialless local-path fetch; pushed objects cross into
// the transport repository the same way.
func runSanitizedGitTransport(ctx context.Context, repositoryDir, remoteURL string, args ...string) (string, error) {
	root, err := os.MkdirTemp("", "hive-repair-git-transport-")
	if err != nil {
		return "", fmt.Errorf("create controller Git transport repository: %w", err)
	}
	defer os.RemoveAll(root)
	if err := os.Chmod(root, 0o700); err != nil {
		return "", fmt.Errorf("protect controller Git transport repository: %w", err)
	}
	if _, err := runGitCommand(ctx, root, "", "init", "--bare", "."); err != nil {
		return "", fmt.Errorf("initialize controller Git transport repository: %w", err)
	}
	if len(args) == 0 {
		return "", fmt.Errorf("controller Git transport command is required")
	}
	switch args[0] {
	case "ls-remote":
		return sanitizedGitLSRemote(ctx, root, remoteURL, args[1:])
	case "fetch":
		return sanitizedGitFetch(ctx, root, repositoryDir, remoteURL, args[1:])
	case "push":
		return sanitizedGitPush(ctx, root, repositoryDir, remoteURL, args[1:])
	default:
		return "", fmt.Errorf("unsupported controller Git transport operation")
	}
}

func sanitizedGitLSRemote(ctx context.Context, transportDir, remoteURL string, args []string) (string, error) {
	if len(args) != 3 || args[0] != "--heads" || args[1] != remoteURL || !validTransportRef(args[2], "refs/heads/") {
		return "", fmt.Errorf("controller ls-remote request is not an exact branch lookup")
	}
	return runGitCommand(ctx, transportDir, remoteURL, "ls-remote", "--heads", remoteURL, args[2])
}

func sanitizedGitFetch(ctx context.Context, transportDir, repositoryDir, remoteURL string, args []string) (string, error) {
	remoteIndex := -1
	for index, arg := range args {
		if arg == remoteURL {
			if remoteIndex >= 0 {
				return "", fmt.Errorf("controller fetch repeats its exact remote URL")
			}
			remoteIndex = index
		}
	}
	if remoteIndex < 0 || remoteIndex+2 != len(args) {
		return "", fmt.Errorf("controller fetch request is not one exact refspec")
	}
	for _, option := range args[:remoteIndex] {
		if option != "--prune" && option != "--no-tags" && option != "--force" {
			return "", fmt.Errorf("controller fetch contains an unsupported option")
		}
	}
	source, destination, err := parseFetchTransportRefspec(args[remoteIndex+1])
	if err != nil {
		return "", err
	}
	output, err := runGitCommand(ctx, transportDir, remoteURL, "fetch", "--no-tags", "--force", remoteURL, "+"+source+":"+transportFetchRef)
	if err != nil {
		return output, err
	}
	expected, err := runGitCommand(ctx, transportDir, "", "rev-parse", "--verify", transportFetchRef+"^{commit}")
	if err != nil || !validGitCommitSHA(strings.TrimSpace(expected)) {
		return output, fmt.Errorf("controller fetch did not produce one exact commit")
	}
	if err := rejectExecutableRepositoryGitConfig(ctx, repositoryDir); err != nil {
		return output, fmt.Errorf("refuse credentialless fetch import into unsafe target repository: %w", err)
	}
	if _, err := runGit(ctx, repositoryDir, "fetch", "--no-tags", "--force", transportDir, "+"+transportFetchRef+":"+destination); err != nil {
		return output, fmt.Errorf("import credentialless controller fetch: %w", err)
	}
	imported, err := runGit(ctx, repositoryDir, "rev-parse", "--verify", destination+"^{commit}")
	if err != nil || !strings.EqualFold(strings.TrimSpace(imported), strings.TrimSpace(expected)) {
		return output, fmt.Errorf("credentialless controller fetch import changed exact commit identity")
	}
	return output, nil
}

func sanitizedGitPush(ctx context.Context, transportDir, repositoryDir, remoteURL string, args []string) (string, error) {
	if len(args) != 3 || !strings.HasPrefix(args[0], "--force-with-lease=refs/heads/") || args[1] != remoteURL {
		return "", fmt.Errorf("controller push request is not one exact leased ref update")
	}
	source, destination, found := strings.Cut(args[2], ":")
	if !found || !validGitCommitSHA(source) || !validTransportRef(destination, "refs/heads/hive/") || strings.Contains(destination, ":") {
		return "", fmt.Errorf("controller push refspec is invalid")
	}
	leaseRef, leaseSHA, found := strings.Cut(strings.TrimPrefix(args[0], "--force-with-lease="), ":")
	if !found || leaseRef != destination || leaseSHA != "" && !validGitCommitSHA(leaseSHA) {
		return "", fmt.Errorf("controller push lease does not bind its exact destination")
	}
	if err := rejectExecutableRepositoryGitConfig(ctx, repositoryDir); err != nil {
		return "", fmt.Errorf("refuse push import from unsafe target repository: %w", err)
	}
	if _, err := runGitCommand(ctx, transportDir, "", "fetch", "--no-tags", "--force", repositoryDir, "+"+source+":"+transportPushRef); err != nil {
		return "", fmt.Errorf("import exact push object into controller transport: %w", err)
	}
	imported, err := runGitCommand(ctx, transportDir, "", "rev-parse", "--verify", transportPushRef+"^{commit}")
	if err != nil || !strings.EqualFold(strings.TrimSpace(imported), source) {
		return "", fmt.Errorf("controller push import changed exact commit identity")
	}
	return runGitCommand(ctx, transportDir, remoteURL, "push", args[0], remoteURL, source+":"+destination)
}

func parseFetchTransportRefspec(refspec string) (string, string, error) {
	refspec = strings.TrimPrefix(refspec, "+")
	source, destination, found := strings.Cut(refspec, ":")
	if !found || strings.Contains(destination, ":") || !validTransportRef(source, "refs/heads/") ||
		!validTransportDestination(destination) {
		return "", "", fmt.Errorf("controller fetch refspec is invalid")
	}
	return source, destination, nil
}

func validTransportDestination(ref string) bool {
	return validTransportRef(ref, "refs/remotes/origin/") || validTransportRef(ref, "refs/hive/")
}

func validTransportRef(ref, prefix string) bool {
	if !strings.HasPrefix(ref, prefix) || len(ref) <= len(prefix) || strings.TrimSpace(ref) != ref {
		return false
	}
	return !strings.ContainsAny(ref, " ~^:?*[\\\r\n\x00") && !strings.Contains(ref, "..") && !strings.Contains(ref, "//") &&
		!strings.HasSuffix(ref, "/") && !strings.HasSuffix(ref, ".") && !strings.HasSuffix(ref, ".lock")
}

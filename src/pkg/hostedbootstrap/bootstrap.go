// Package hostedbootstrap initializes the repository-owned control-plane state
// used by Hive's hosted controller.
//
// Bootstrap is deliberately create-only. It never reads secret values, never
// returns the newly generated key, and refuses to repair a preexisting
// branch/secret pair when only one member exists. It removes only the exact
// branch created by the current failed invocation while the secret is proven
// absent. That fail-closed behavior prevents a setup retry from silently
// rotating the key that authenticates durable hosted state.
package hostedbootstrap

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"

	"github.com/google/go-github/v72/github"
	"github.com/kubestellar/hive/pkg/hostedstate"
	"golang.org/x/crypto/nacl/box"
)

const (
	// StateSecretName is the only repository secret managed by this package.
	StateSecretName = "HIVE_HOSTED_STATE_KEY"
	// StateFileName is the complete initial tree of the orphan state branch.
	StateFileName = "state.json"

	hmacKeyBytes = 32
)

var (
	ownerOrRepositoryPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)
	stateBranchPattern       = regexp.MustCompile(`^hive/state-[A-Za-z0-9][A-Za-z0-9._-]{0,95}$`)
	releaseVersionPattern    = regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+-integrated\.[0-9]+$`)
	releaseDigestPattern     = regexp.MustCompile(`^[a-f0-9]{64}$`)
)

// Options binds a bootstrap to one repository and one prepared, private state
// root. StatePaths and Metadata are passed to hostedstate.Export unchanged
// after repository and branch identity checks.
type Options struct {
	Owner          string
	Repository     string
	StateBranch    string
	StateRoot      string
	StatePaths     []string
	Metadata       hostedstate.Metadata
	RestoreRelease *hostedstate.ReleaseIdentity
	Limits         hostedstate.Limits
}

// Result intentionally contains no secret material or encrypted secret value.
type Result struct {
	Repository        string `json:"repository"`
	StateBranch       string `json:"stateBranch"`
	StateCommitSHA    string `json:"stateCommitSha"`
	SecretName        string `json:"secretName"`
	AlreadyConfigured bool   `json:"alreadyConfigured"`
}

// Bootstrap creates the initial orphan state branch and its matching Actions
// secret. Existing complete installations are an identity-checked no-op.
// Preexisting partial installations are never guessed at, deleted, overwritten,
// or rotated.
func Bootstrap(ctx context.Context, client *github.Client, opts Options) (Result, error) {
	return bootstrap(ctx, client, opts, rand.Reader)
}

func bootstrap(ctx context.Context, client *github.Client, opts Options, random io.Reader) (Result, error) {
	if client == nil {
		return Result{}, errors.New("GitHub client is required")
	}
	if random == nil {
		return Result{}, errors.New("cryptographic random source is required")
	}
	if err := validateOptions(opts); err != nil {
		return Result{}, err
	}

	result := Result{
		Repository:  opts.Owner + "/" + opts.Repository,
		StateBranch: opts.StateBranch,
		SecretName:  StateSecretName,
	}

	presence, err := inspectPresence(ctx, client, opts)
	if err != nil {
		return Result{}, err
	}
	if presence.secret && presence.branch {
		if err := verifyExistingState(ctx, client, opts, presence.commitSHA); err != nil {
			return Result{}, err
		}
		result.StateCommitSHA = presence.commitSHA
		result.AlreadyConfigured = true
		return result, nil
	}
	if presence.secret != presence.branch {
		return Result{}, inconsistentStateError(presence)
	}

	rawSecret := make([]byte, hmacKeyBytes)
	if _, err := io.ReadFull(random, rawSecret); err != nil {
		return Result{}, fmt.Errorf("generate hosted-state authentication key: %w", err)
	}
	defer zero(rawSecret)
	// GitHub Actions secrets are transported through environment variables.
	// Encode the full random key as printable ASCII so NUL or invalid UTF-8 can
	// never truncate the controller's HMAC key.
	secret := []byte(base64.RawStdEncoding.EncodeToString(rawSecret))
	defer zero(secret)

	bundle, err := hostedstate.Export(hostedstate.ExportOptions{
		Root:     opts.StateRoot,
		Paths:    append([]string(nil), opts.StatePaths...),
		Metadata: opts.Metadata,
		Secret:   secret,
		Limits:   opts.Limits,
	})
	if err != nil {
		return Result{}, fmt.Errorf("prepare initial hosted-state bundle: %w", err)
	}

	tree, _, err := client.Git.CreateTree(ctx, opts.Owner, opts.Repository, "", []*github.TreeEntry{{
		Path:    github.Ptr(StateFileName),
		Mode:    github.Ptr("100644"),
		Type:    github.Ptr("blob"),
		Content: github.Ptr(string(bundle)),
	}})
	if err != nil {
		return Result{}, fmt.Errorf("create initial hosted-state tree: %w", err)
	}
	if tree == nil || !validGitOID(tree.GetSHA()) {
		return Result{}, errors.New("GitHub returned an invalid initial hosted-state tree identity")
	}

	commit, _, err := client.Git.CreateCommit(ctx, opts.Owner, opts.Repository, &github.Commit{
		Message: github.Ptr("Initialize Hive hosted state"),
		Tree:    &github.Tree{SHA: tree.SHA},
		Parents: []*github.Commit{},
	}, nil)
	if err != nil {
		return Result{}, fmt.Errorf("create initial hosted-state commit: %w", err)
	}
	if commit == nil || !validGitOID(commit.GetSHA()) {
		return Result{}, errors.New("GitHub returned an invalid initial hosted-state commit identity")
	}
	result.StateCommitSHA = commit.GetSHA()

	createdRef, _, createRefErr := client.Git.CreateRef(ctx, opts.Owner, opts.Repository, &github.Reference{
		Ref: github.Ptr("refs/heads/" + opts.StateBranch),
		Object: &github.GitObject{
			Type: github.Ptr("commit"),
			SHA:  github.Ptr(commit.GetSHA()),
		},
	})
	if createRefErr != nil {
		// A response can be lost after GitHub accepted the create. Reconcile
		// without mutating: only our exact commit permits this invocation to
		// continue. A concurrently completed bootstrap is an idempotent success.
		reconciled, inspectErr := inspectPresence(ctx, client, opts)
		if inspectErr != nil {
			return Result{}, fmt.Errorf("create hosted-state branch: %v; reconcile: %w", createRefErr, inspectErr)
		}
		if reconciled.secret && reconciled.branch {
			if verifyErr := verifyExistingState(ctx, client, opts, reconciled.commitSHA); verifyErr != nil {
				return Result{}, fmt.Errorf("create hosted-state branch: %v; reconcile hosted-state metadata: %w", createRefErr, verifyErr)
			}
			result.StateCommitSHA = reconciled.commitSHA
			result.AlreadyConfigured = true
			return result, nil
		}
		if !reconciled.secret && reconciled.branch && reconciled.commitSHA == commit.GetSHA() {
			// This invocation owns the exact branch content; continue with the
			// create-only secret step.
		} else if reconciled.secret != reconciled.branch {
			return Result{}, fmt.Errorf("create hosted-state branch: %v; %w", createRefErr, inconsistentStateError(reconciled))
		} else {
			return Result{}, fmt.Errorf("create hosted-state branch: %w", createRefErr)
		}
	} else if err := validateCreatedRef(createdRef, opts.StateBranch, commit.GetSHA()); err != nil {
		return Result{}, err
	}

	// The Actions API exposes only create-or-update for repository secrets.
	// Recheck after winning the create-only branch operation and immediately
	// before the PUT. Hive will never intentionally invoke PUT for a secret that
	// GitHub reports as existing.
	secretExists, err := repoSecretExists(ctx, client, opts.Owner, opts.Repository)
	if err != nil {
		return Result{}, abortOwnedBranch(ctx, client, opts, commit.GetSHA(), err)
	}
	if secretExists {
		return Result{}, errors.New("hosted-state branch was created but repository secret appeared concurrently; refusing to overwrite or rotate it")
	}

	publicKey, _, err := client.Actions.GetRepoPublicKey(ctx, opts.Owner, opts.Repository)
	if err != nil {
		return Result{}, abortOwnedBranch(ctx, client, opts, commit.GetSHA(), fmt.Errorf("get repository Actions public key: %w", err))
	}
	if publicKey == nil || strings.TrimSpace(publicKey.GetKeyID()) == "" {
		return Result{}, abortOwnedBranch(ctx, client, opts, commit.GetSHA(), errors.New("GitHub returned an invalid repository Actions public-key identity"))
	}
	encrypted, err := sealForGitHub(publicKey.GetKey(), secret, random)
	if err != nil {
		return Result{}, abortOwnedBranch(ctx, client, opts, commit.GetSHA(), err)
	}
	if _, err := client.Actions.CreateOrUpdateRepoSecret(ctx, opts.Owner, opts.Repository, &github.EncryptedSecret{
		Name:           StateSecretName,
		KeyID:          publicKey.GetKeyID(),
		EncryptedValue: encrypted,
	}); err != nil {
		cause := fmt.Errorf("create repository hosted-state secret: %w", err)
		reconciled, inspectErr := inspectPresence(ctx, client, opts)
		if inspectErr == nil && reconciled.secret && reconciled.branch && reconciled.commitSHA == commit.GetSHA() {
			if verifyErr := verifyExistingState(ctx, client, opts, reconciled.commitSHA); verifyErr != nil {
				return Result{}, fmt.Errorf("%v; reconcile hosted-state metadata: %w", cause, verifyErr)
			}
			// GitHub accepted the secret but its response was lost. The exact
			// branch and metadata prove this invocation's installation completed.
			return result, nil
		}
		if inspectErr != nil {
			cause = fmt.Errorf("%v; reconcile: %w", cause, inspectErr)
		}
		return Result{}, abortOwnedBranch(ctx, client, opts, commit.GetSHA(), cause)
	}

	return result, nil
}

// verifyExistingState reads state.json through the exact commit named by the
// state ref. The repository secret is intentionally unreadable, so this check
// cannot authenticate the HMAC. It does fail closed on a malformed bundle or
// an installation identity that differs from the setup request before an
// existing branch/secret pair is treated as an idempotent no-op.
func verifyExistingState(ctx context.Context, client *github.Client, opts Options, commitSHA string) error {
	if !validGitOID(commitSHA) {
		return errors.New("existing hosted-state commit identity is invalid")
	}
	content, directory, _, err := client.Repositories.GetContents(ctx, opts.Owner, opts.Repository, StateFileName, &github.RepositoryContentGetOptions{Ref: commitSHA})
	if err != nil {
		return fmt.Errorf("read existing hosted state at commit %s: %w", commitSHA, err)
	}
	if content == nil || directory != nil || content.GetType() != "file" || content.GetPath() != StateFileName || !validGitOID(content.GetSHA()) {
		return errors.New("GitHub returned invalid existing hosted-state file metadata")
	}
	encoded, err := content.GetContent()
	if err != nil {
		return fmt.Errorf("decode existing hosted state: %w", err)
	}
	bundle, err := hostedstate.Decode([]byte(encoded))
	if err != nil {
		return fmt.Errorf("decode existing hosted-state bundle: %w", err)
	}
	if bundle.SchemaVersion != hostedstate.SchemaVersion {
		return fmt.Errorf("existing hosted state uses unsupported schema %q", bundle.SchemaVersion)
	}
	if err := validateSignatureShape(bundle.Signature); err != nil {
		return err
	}
	actual, expected := bundle.Metadata, opts.Metadata
	if actual.Repository != expected.Repository {
		return errors.New("existing hosted-state repository identity does not match setup")
	}
	if actual.StateBranch != opts.StateBranch {
		return errors.New("existing hosted-state branch identity does not match setup")
	}
	if actual.Release != expected.Release && (opts.RestoreRelease == nil || actual.Release != *opts.RestoreRelease) {
		return errors.New("existing hosted-state release identity does not match setup")
	}
	if actual.Controller.WorkflowPath != expected.Controller.WorkflowPath {
		return errors.New("existing hosted-state controller workflow path does not match setup")
	}
	switch actual.ExecutionOwner {
	case "bootstrap":
		if actual.Sequence != expected.Sequence || actual.PriorStateCommit != expected.PriorStateCommit || actual.Controller != expected.Controller {
			return errors.New("existing hosted-state bootstrap controller binding does not match setup")
		}
	case "hosted":
		if actual.Sequence < expected.Sequence || actual.Sequence == 0 || actual.PriorStateCommit == "" || !validGitOID(actual.PriorStateCommit) || actual.Controller.RunID == 0 || actual.Controller.Attempt == 0 || (actual.Controller.Event != "schedule" && actual.Controller.Event != "workflow_dispatch") || !validGitOID(actual.Controller.WorkflowSHA) || !validGitOID(actual.Controller.DefaultHeadSHA) {
			return errors.New("existing hosted-state controller binding is malformed")
		}
	default:
		return errors.New("existing hosted-state execution owner is invalid")
	}
	return nil
}

func validateSignatureShape(value string) error {
	const prefix = "hmac-sha256:"
	if !strings.HasPrefix(value, prefix) {
		return errors.New("existing hosted-state signature has an unsupported algorithm")
	}
	decoded, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(value, prefix))
	if err != nil || len(decoded) != 32 {
		return errors.New("existing hosted-state signature is malformed")
	}
	return nil
}

// abortOwnedBranch is the narrow rollback for a bootstrap that created its
// exact commit but could not create the matching secret. Preexisting partial
// state never reaches this helper. Two consecutive ref reads bound response
// loss and detectable concurrent advancement; a changed ref or an existing
// secret is always preserved.
func abortOwnedBranch(ctx context.Context, client *github.Client, opts Options, ownedCommit string, cause error) error {
	secret, err := repoSecretExists(ctx, client, opts.Owner, opts.Repository)
	if err != nil {
		return fmt.Errorf("%v; cannot safely clean up hosted-state branch because secret presence could not be reconciled: %w", cause, err)
	}
	if secret {
		return fmt.Errorf("%v; repository secret now exists, so the hosted-state branch was preserved", cause)
	}
	for check := 0; check < 2; check++ {
		exists, current, err := stateBranch(ctx, client, opts.Owner, opts.Repository, opts.StateBranch)
		if err != nil {
			return fmt.Errorf("%v; cannot safely clean up hosted-state branch: %w", cause, err)
		}
		if !exists {
			return cause
		}
		if current != ownedCommit {
			return fmt.Errorf("%v; hosted-state branch advanced to %s and was preserved", cause, current)
		}
	}
	_, err = client.Git.DeleteRef(ctx, opts.Owner, opts.Repository, "refs/heads/"+opts.StateBranch)
	if err == nil {
		return fmt.Errorf("%v; removed the exact incomplete hosted-state branch so setup can be retried", cause)
	}
	// A lost delete response is safe to accept only when the ref is absent and
	// the secret is still absent. Never retry a delete after an ambiguous error.
	presence, inspectErr := inspectPresence(ctx, client, opts)
	if inspectErr == nil && !presence.secret && !presence.branch {
		return fmt.Errorf("%v; removed the exact incomplete hosted-state branch so setup can be retried", cause)
	}
	if inspectErr != nil {
		return fmt.Errorf("%v; delete incomplete hosted-state branch: %v; reconcile: %w", cause, err, inspectErr)
	}
	return fmt.Errorf("%v; delete incomplete hosted-state branch: %w", cause, err)
}

type presence struct {
	secret    bool
	branch    bool
	commitSHA string
}

func inspectPresence(ctx context.Context, client *github.Client, opts Options) (presence, error) {
	secret, err := repoSecretExists(ctx, client, opts.Owner, opts.Repository)
	if err != nil {
		return presence{}, err
	}
	branch, commitSHA, err := stateBranch(ctx, client, opts.Owner, opts.Repository, opts.StateBranch)
	if err != nil {
		return presence{}, err
	}
	return presence{secret: secret, branch: branch, commitSHA: commitSHA}, nil
}

func repoSecretExists(ctx context.Context, client *github.Client, owner, repository string) (bool, error) {
	secret, response, err := client.Actions.GetRepoSecret(ctx, owner, repository, StateSecretName)
	if err != nil {
		if isNotFound(response, err) {
			return false, nil
		}
		return false, fmt.Errorf("inspect repository hosted-state secret: %w", err)
	}
	if secret == nil || secret.Name != StateSecretName {
		return false, errors.New("GitHub returned inconsistent hosted-state secret metadata")
	}
	return true, nil
}

func stateBranch(ctx context.Context, client *github.Client, owner, repository, branch string) (bool, string, error) {
	ref, response, err := client.Git.GetRef(ctx, owner, repository, "refs/heads/"+branch)
	if err != nil {
		if isNotFound(response, err) {
			return false, "", nil
		}
		return false, "", fmt.Errorf("inspect hosted-state branch: %w", err)
	}
	if ref == nil || ref.Object == nil || ref.Object.GetType() != "commit" || !validGitOID(ref.Object.GetSHA()) {
		return false, "", errors.New("GitHub returned invalid hosted-state branch metadata")
	}
	want := "refs/heads/" + branch
	if ref.GetRef() != "" && ref.GetRef() != want {
		return false, "", fmt.Errorf("GitHub returned hosted-state ref %q, expected %q", ref.GetRef(), want)
	}
	return true, ref.Object.GetSHA(), nil
}

func inconsistentStateError(value presence) error {
	switch {
	case value.secret && !value.branch:
		return errors.New("hosted-state repository secret exists without its state branch; refusing to rotate or reconstruct state")
	case value.branch && !value.secret:
		return errors.New("hosted-state branch exists without its repository secret; refusing to overwrite or invent authentication state")
	default:
		return errors.New("hosted-state bootstrap metadata is inconsistent")
	}
}

func validateOptions(opts Options) error {
	if !ownerOrRepositoryPattern.MatchString(opts.Owner) || !ownerOrRepositoryPattern.MatchString(opts.Repository) || opts.Owner == "." || opts.Owner == ".." || opts.Repository == "." || opts.Repository == ".." {
		return errors.New("GitHub owner and repository must be non-empty canonical names")
	}
	if !stateBranchPattern.MatchString(opts.StateBranch) || strings.Contains(opts.StateBranch, "..") || strings.HasSuffix(opts.StateBranch, ".") {
		return errors.New("hosted state branch must use the bounded hive/state-<repository-id> form")
	}
	wantRepository := opts.Owner + "/" + opts.Repository
	if opts.Metadata.Repository.FullName != wantRepository {
		return fmt.Errorf("hosted-state metadata repository %q does not match target %q", opts.Metadata.Repository.FullName, wantRepository)
	}
	if opts.Metadata.StateBranch != opts.StateBranch {
		return errors.New("hosted-state metadata branch does not match the requested state branch")
	}
	if strings.TrimSpace(opts.StateRoot) == "" {
		return errors.New("prepared hosted-state root is required")
	}
	if opts.RestoreRelease != nil {
		if !validHostedReleaseIdentity(*opts.RestoreRelease) || *opts.RestoreRelease == opts.Metadata.Release {
			return errors.New("hosted-state predecessor release identity is invalid")
		}
	}
	return nil
}

func validHostedReleaseIdentity(identity hostedstate.ReleaseIdentity) bool {
	return releaseVersionPattern.MatchString(identity.Version) && validGitOID(identity.HiveCommit) && validGitOID(identity.VisualHiveCommit) &&
		releaseDigestPattern.MatchString(identity.DistributionManifestSHA256) && identity.HostedControllerProtocol == 1
}

func validateCreatedRef(ref *github.Reference, branch, commitSHA string) error {
	if ref == nil || ref.Object == nil || ref.GetRef() != "refs/heads/"+branch || ref.Object.GetType() != "commit" || ref.Object.GetSHA() != commitSHA {
		return errors.New("GitHub returned a hosted-state branch that does not match the requested commit")
	}
	return nil
}

func sealForGitHub(encodedPublicKey string, plaintext []byte, random io.Reader) (string, error) {
	decoded, err := base64.StdEncoding.DecodeString(encodedPublicKey)
	if err != nil || len(decoded) != 32 {
		return "", errors.New("GitHub repository Actions public key is malformed")
	}
	var publicKey [32]byte
	copy(publicKey[:], decoded)
	sealed, err := box.SealAnonymous(nil, plaintext, &publicKey, random)
	if err != nil {
		return "", fmt.Errorf("encrypt repository hosted-state secret: %w", err)
	}
	return base64.StdEncoding.EncodeToString(sealed), nil
}

func isNotFound(response *github.Response, err error) bool {
	if response != nil && response.Response != nil && response.StatusCode == http.StatusNotFound {
		return true
	}
	var githubError *github.ErrorResponse
	return errors.As(err, &githubError) && githubError.Response != nil && githubError.Response.StatusCode == http.StatusNotFound
}

func validGitOID(value string) bool {
	if len(value) != 40 && len(value) != 64 {
		return false
	}
	for _, character := range value {
		if !((character >= '0' && character <= '9') || (character >= 'a' && character <= 'f')) {
			return false
		}
	}
	return true
}

func zero(value []byte) {
	for index := range value {
		value[index] = 0
	}
}

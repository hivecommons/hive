package hostedbootstrap

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"regexp"
	"strings"

	"github.com/google/go-github/v72/github"
	"golang.org/x/crypto/nacl/box"
)

const (
	// ProviderSecretName is the create-only repository secret used by the
	// hosted Codex provider. Hive never reads, returns, or rotates its value.
	ProviderSecretName = "OPENAI_API_KEY"

	// GitHub documents a 48 KB limit for an encrypted Actions secret. The
	// provider credential is intentionally bounded well below that limit so a
	// setup request cannot turn secret provisioning into an unbounded
	// allocation or request.
	providerSecretMaxBytes = 16 * 1024
)

var providerKeyIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

// ProviderSecretResult contains metadata only. It intentionally cannot carry
// the provider credential or its sealed ciphertext.
type ProviderSecretResult struct {
	SecretName        string `json:"secretName"`
	AlreadyConfigured bool   `json:"alreadyConfigured"`
}

// EnsureProviderSecret provisions OPENAI_API_KEY exactly once. GitHub does
// not expose a create-only secret endpoint, so Hive checks metadata before
// loading the value, checks it again immediately before PUT, and refuses to
// overwrite a secret that appears concurrently. A failed PUT is reconciled by
// metadata only to account for a response lost after GitHub accepted it.
//
// The caller retains ownership of plaintext. Hive makes one short-lived copy
// and wipes that copy, the decoded public key, and sealed byte buffers before
// returning.
func EnsureProviderSecret(ctx context.Context, client *github.Client, owner, repository string, plaintext []byte) (ProviderSecretResult, error) {
	result := ProviderSecretResult{SecretName: ProviderSecretName}
	if client == nil {
		return ProviderSecretResult{}, errors.New("GitHub client is required")
	}
	if err := validateRepositoryTarget(owner, repository); err != nil {
		return ProviderSecretResult{}, err
	}

	exists, err := namedRepoSecretExists(ctx, client, owner, repository, ProviderSecretName)
	if err != nil {
		return ProviderSecretResult{}, err
	}
	if exists {
		result.AlreadyConfigured = true
		return result, nil
	}
	if len(plaintext) == 0 {
		return ProviderSecretResult{}, errors.New("hosted provider credential is required when OPENAI_API_KEY is not configured")
	}
	if len(plaintext) > providerSecretMaxBytes {
		return ProviderSecretResult{}, errors.New("hosted provider credential exceeds the supported size limit")
	}

	secret := append([]byte(nil), plaintext...)
	defer zero(secret)

	publicKey, _, err := client.Actions.GetRepoPublicKey(ctx, owner, repository)
	if err != nil {
		return ProviderSecretResult{}, errors.New("get repository Actions public key for hosted provider secret")
	}
	if publicKey == nil || !providerKeyIDPattern.MatchString(strings.TrimSpace(publicKey.GetKeyID())) {
		return ProviderSecretResult{}, errors.New("GitHub returned an invalid repository Actions public-key identity")
	}
	encrypted, err := sealProviderSecret(publicKey.GetKey(), secret)
	if err != nil {
		return ProviderSecretResult{}, err
	}
	defer zero(encrypted)

	// This is deliberately the last request before PUT. Never call the
	// create-or-update endpoint after GitHub reports existing metadata.
	exists, err = namedRepoSecretExists(ctx, client, owner, repository, ProviderSecretName)
	if err != nil {
		return ProviderSecretResult{}, err
	}
	if exists {
		return ProviderSecretResult{}, errors.New("OPENAI_API_KEY appeared concurrently; refusing to overwrite or rotate it")
	}

	request := &github.EncryptedSecret{
		Name:           ProviderSecretName,
		KeyID:          strings.TrimSpace(publicKey.GetKeyID()),
		EncryptedValue: string(encrypted),
	}
	_, putErr := client.Actions.CreateOrUpdateRepoSecret(ctx, owner, repository, request)
	// Drop the only explicitly retained ciphertext string as soon as the
	// request completes. The byte backing buffer is wiped by the defer above.
	request.EncryptedValue = ""
	if putErr == nil {
		return result, nil
	}

	// The API may have accepted the secret and lost its response. Reconcile by
	// unreadable metadata; never retry PUT and therefore never rotate a value.
	exists, reconcileErr := namedRepoSecretExists(ctx, client, owner, repository, ProviderSecretName)
	if reconcileErr == nil && exists {
		return result, nil
	}
	if reconcileErr != nil {
		return ProviderSecretResult{}, errors.New("create repository hosted provider secret failed and metadata reconciliation was unavailable")
	}
	return ProviderSecretResult{}, errors.New("create repository hosted provider secret failed")
}

func validateRepositoryTarget(owner, repository string) error {
	if !ownerOrRepositoryPattern.MatchString(owner) || !ownerOrRepositoryPattern.MatchString(repository) || owner == "." || owner == ".." || repository == "." || repository == ".." {
		return errors.New("GitHub owner and repository must be non-empty canonical names")
	}
	return nil
}

func namedRepoSecretExists(ctx context.Context, client *github.Client, owner, repository, name string) (bool, error) {
	secret, response, err := client.Actions.GetRepoSecret(ctx, owner, repository, name)
	if err != nil {
		if isNotFound(response, err) {
			return false, nil
		}
		return false, errors.New("inspect repository secret metadata")
	}
	if secret == nil || secret.Name != name {
		return false, errors.New("GitHub returned inconsistent repository secret metadata")
	}
	return true, nil
}

func sealProviderSecret(encodedPublicKey string, plaintext []byte) ([]byte, error) {
	decoded, err := base64.StdEncoding.DecodeString(encodedPublicKey)
	if err != nil || len(decoded) != 32 {
		zero(decoded)
		return nil, errors.New("GitHub repository Actions public key is malformed")
	}
	defer zero(decoded)

	var publicKey [32]byte
	copy(publicKey[:], decoded)
	defer zero(publicKey[:])

	sealed, err := box.SealAnonymous(nil, plaintext, &publicKey, rand.Reader)
	if err != nil {
		zero(sealed)
		return nil, errors.New("encrypt repository hosted provider secret")
	}
	encoded := make([]byte, base64.StdEncoding.EncodedLen(len(sealed)))
	base64.StdEncoding.Encode(encoded, sealed)
	zero(sealed)
	return encoded, nil
}

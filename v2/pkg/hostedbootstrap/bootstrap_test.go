package hostedbootstrap

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-github/v72/github"
	"github.com/kubestellar/hive/v2/pkg/hostedstate"
	"golang.org/x/crypto/nacl/box"
)

const (
	testTreeSHA   = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	testCommitSHA = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

func TestBootstrapCreatesOrphanSignedStateAndEncryptedSecret(t *testing.T) {
	harness := newAPIHarness(t)
	server, client := harness.serverAndClient(t)
	defer server.Close()

	opts := testOptions(t)
	result, err := Bootstrap(context.Background(), client, opts)
	if err != nil {
		t.Fatalf("Bootstrap() error = %v", err)
	}
	if result.AlreadyConfigured {
		t.Fatal("new bootstrap reported already configured")
	}
	if result.Repository != "acme/widget" || result.StateBranch != opts.StateBranch || result.StateCommitSHA != testCommitSHA || result.SecretName != StateSecretName {
		t.Fatalf("unexpected result: %+v", result)
	}

	harness.mu.Lock()
	secret := append([]byte(nil), harness.decryptedSecret...)
	bundle := append([]byte(nil), harness.stateBundle...)
	events := append([]string(nil), harness.events...)
	commitRequest := append([]byte(nil), harness.commitRequest...)
	harness.mu.Unlock()

	decodedSecret, decodeErr := base64.RawStdEncoding.DecodeString(string(secret))
	if decodeErr != nil || len(decodedSecret) != hmacKeyBytes {
		t.Fatalf("decrypted secret is not a printable encoding of %d random bytes: bytes=%d err=%v", hmacKeyBytes, len(secret), decodeErr)
	}
	if indexRef, indexSecret := slices.Index(events, "create-ref"), slices.Index(events, "put-secret"); indexRef < 0 || indexSecret < 0 || indexRef >= indexSecret {
		t.Fatalf("state branch must win create-only ownership before secret write; events=%v", events)
	}
	var commitBody map[string]any
	if err := json.Unmarshal(commitRequest, &commitBody); err != nil {
		t.Fatalf("decode commit request: %v", err)
	}
	if _, exists := commitBody["parents"]; exists {
		t.Fatalf("initial state commit must be orphaned, request=%s", commitRequest)
	}

	restoreParent := t.TempDir()
	restorePath := filepath.Join(restoreParent, "restored")
	if err := hostedstate.Restore(hostedstate.RestoreOptions{
		Bundle:      bundle,
		Destination: restorePath,
		Secret:      secret,
		Expected: hostedstate.ExpectedState{
			Repository:       opts.Metadata.Repository,
			StateBranch:      opts.StateBranch,
			Sequence:         opts.Metadata.Sequence,
			PriorStateCommit: opts.Metadata.PriorStateCommit,
			Release:          opts.Metadata.Release,
		},
	}); err != nil {
		t.Fatalf("created state bundle did not authenticate and restore: %v", err)
	}
	restored, err := os.ReadFile(filepath.Join(restorePath, "lifecycle", "state.json"))
	if err != nil {
		t.Fatalf("read restored state: %v", err)
	}
	if string(restored) != `{"phase":"initialized"}` {
		t.Fatalf("restored state = %q", restored)
	}
}

func TestBootstrapIsIdempotentWhenSecretAndBranchExist(t *testing.T) {
	harness := newAPIHarness(t)
	harness.secretExists = true
	harness.branchSHA = strings.Repeat("c", 40)
	opts := testOptions(t)
	harness.stateBundle = bundleForMetadata(t, opts, opts.Metadata)
	server, client := harness.serverAndClient(t)
	defer server.Close()

	result, err := bootstrap(context.Background(), client, opts, errorReader{})
	if err != nil {
		t.Fatalf("Bootstrap() error = %v", err)
	}
	if !result.AlreadyConfigured || result.StateCommitSHA != harness.branchSHA {
		t.Fatalf("unexpected idempotent result: %+v", result)
	}
	harness.mu.Lock()
	defer harness.mu.Unlock()
	if harness.mutationCount != 0 {
		t.Fatalf("idempotent bootstrap made %d mutations", harness.mutationCount)
	}
}

func TestBootstrapIsIdempotentAfterHostedStateAdvances(t *testing.T) {
	opts := testOptions(t)
	metadata := opts.Metadata
	metadata.Sequence = 2
	metadata.PriorStateCommit = strings.Repeat("a", 40)
	metadata.ExecutionOwner = "hosted"
	metadata.Controller.RunID = 42
	metadata.Controller.Attempt = 1
	metadata.Controller.Event = "schedule"
	metadata.Controller.WorkflowSHA = strings.Repeat("3", 40)
	metadata.Controller.DefaultHeadSHA = strings.Repeat("4", 40)

	harness := newAPIHarness(t)
	harness.secretExists = true
	harness.branchSHA = strings.Repeat("c", 40)
	harness.stateBundle = bundleForMetadata(t, opts, metadata)
	server, client := harness.serverAndClient(t)
	defer server.Close()

	result, err := bootstrap(context.Background(), client, opts, errorReader{})
	if err != nil {
		t.Fatalf("Bootstrap() error = %v", err)
	}
	if !result.AlreadyConfigured || result.StateCommitSHA != harness.branchSHA {
		t.Fatalf("unexpected advanced idempotent result: %+v", result)
	}
}

func TestBootstrapRejectsCompleteInstallationWithWrongMetadata(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*hostedstate.Metadata)
		want   string
	}{
		{name: "repository", mutate: func(value *hostedstate.Metadata) { value.Repository.ID++ }, want: "repository identity"},
		{name: "release", mutate: func(value *hostedstate.Metadata) { value.Release.Version = "v0.4.1-integrated.20" }, want: "release identity"},
		{name: "release manifest", mutate: func(value *hostedstate.Metadata) { value.Release.DistributionManifestSHA256 = strings.Repeat("9", 64) }, want: "release identity"},
		{name: "workflow", mutate: func(value *hostedstate.Metadata) { value.Controller.WorkflowPath = ".github/workflows/other.yml" }, want: "workflow path"},
		{name: "bootstrap binding", mutate: func(value *hostedstate.Metadata) { value.Controller.DefaultHeadSHA = strings.Repeat("2", 40) }, want: "bootstrap controller binding"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			opts := testOptions(t)
			metadata := opts.Metadata
			test.mutate(&metadata)
			harness := newAPIHarness(t)
			harness.secretExists = true
			harness.branchSHA = strings.Repeat("c", 40)
			harness.stateBundle = bundleForMetadata(t, opts, metadata)
			server, client := harness.serverAndClient(t)
			defer server.Close()

			_, err := bootstrap(context.Background(), client, opts, errorReader{})
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Bootstrap() error = %v, want containing %q", err, test.want)
			}
			harness.mu.Lock()
			defer harness.mu.Unlock()
			if harness.mutationCount != 0 {
				t.Fatalf("wrong-metadata bootstrap made %d mutations", harness.mutationCount)
			}
		})
	}
}

func TestBootstrapCleansExactBranchWhenSecretCreationCannotStart(t *testing.T) {
	harness := newAPIHarness(t)
	harness.invalidPublicKey = true
	server, client := harness.serverAndClient(t)
	defer server.Close()

	_, err := Bootstrap(context.Background(), client, testOptions(t))
	if err == nil || !strings.Contains(err.Error(), "removed the exact incomplete hosted-state branch") {
		t.Fatalf("Bootstrap() error = %v, want bounded cleanup evidence", err)
	}
	harness.mu.Lock()
	defer harness.mu.Unlock()
	if harness.branchSHA != "" || harness.deleteRefCount != 1 || harness.secretPutCount != 0 {
		t.Fatalf("cleanup state branch=%q deletes=%d secret puts=%d", harness.branchSHA, harness.deleteRefCount, harness.secretPutCount)
	}
}

func TestBootstrapCleansExactBranchWhenSecretPutFails(t *testing.T) {
	harness := newAPIHarness(t)
	harness.secretPutStatus = http.StatusInternalServerError
	server, client := harness.serverAndClient(t)
	defer server.Close()

	_, err := Bootstrap(context.Background(), client, testOptions(t))
	if err == nil || !strings.Contains(err.Error(), "removed the exact incomplete hosted-state branch") {
		t.Fatalf("Bootstrap() error = %v, want bounded cleanup evidence", err)
	}
	harness.mu.Lock()
	defer harness.mu.Unlock()
	if harness.branchSHA != "" || harness.deleteRefCount != 1 || harness.secretPutCount != 1 || harness.secretExists {
		t.Fatalf("cleanup state branch=%q deletes=%d secret puts=%d secret=%v", harness.branchSHA, harness.deleteRefCount, harness.secretPutCount, harness.secretExists)
	}
}

func TestBootstrapPreservesBranchThatAdvancedBeforeCleanup(t *testing.T) {
	harness := newAPIHarness(t)
	harness.invalidPublicKey = true
	advanced := strings.Repeat("9", 40)
	// Initial absence, the first cleanup ownership check, then concurrent
	// advancement before the second ownership check.
	harness.branchSequence = []string{"", testCommitSHA, advanced}
	server, client := harness.serverAndClient(t)
	defer server.Close()

	_, err := Bootstrap(context.Background(), client, testOptions(t))
	if err == nil || !strings.Contains(err.Error(), "advanced to "+advanced) {
		t.Fatalf("Bootstrap() error = %v, want advanced-branch preservation", err)
	}
	harness.mu.Lock()
	defer harness.mu.Unlock()
	if harness.deleteRefCount != 0 || harness.branchSHA != advanced {
		t.Fatalf("advanced branch was mutated: branch=%q deletes=%d", harness.branchSHA, harness.deleteRefCount)
	}
}

func TestBootstrapReconcilesLostSecretPutResponse(t *testing.T) {
	harness := newAPIHarness(t)
	harness.secretPutStatus = http.StatusInternalServerError
	harness.secretPutPersistsOnError = true
	server, client := harness.serverAndClient(t)
	defer server.Close()

	result, err := Bootstrap(context.Background(), client, testOptions(t))
	if err != nil {
		t.Fatalf("Bootstrap() error = %v", err)
	}
	if result.StateCommitSHA != testCommitSHA || result.AlreadyConfigured {
		t.Fatalf("unexpected reconciled result: %+v", result)
	}
	harness.mu.Lock()
	defer harness.mu.Unlock()
	if harness.deleteRefCount != 0 || !harness.secretExists {
		t.Fatalf("completed response-loss bootstrap was not preserved: secret=%v deletes=%d", harness.secretExists, harness.deleteRefCount)
	}
}

func TestBootstrapReconcilesConcurrentCompleteInstallAfterLostRefResponse(t *testing.T) {
	harness := newAPIHarness(t)
	harness.createReferenceStatus = http.StatusInternalServerError
	harness.createRefPersistsOnError = true
	harness.secretSequence = []bool{false, true}
	server, client := harness.serverAndClient(t)
	defer server.Close()

	result, err := Bootstrap(context.Background(), client, testOptions(t))
	if err != nil {
		t.Fatalf("Bootstrap() error = %v", err)
	}
	if !result.AlreadyConfigured || result.StateCommitSHA != testCommitSHA {
		t.Fatalf("unexpected reconciled result: %+v", result)
	}
	harness.mu.Lock()
	defer harness.mu.Unlock()
	if harness.secretPutCount != 0 || harness.deleteRefCount != 0 {
		t.Fatalf("reconciled complete install was mutated: puts=%d deletes=%d", harness.secretPutCount, harness.deleteRefCount)
	}
}

func TestBootstrapFailsClosedOnPartialState(t *testing.T) {
	tests := []struct {
		name         string
		secretExists bool
		branchSHA    string
		want         string
	}{
		{name: "secret only", secretExists: true, want: "secret exists without its state branch"},
		{name: "branch only", branchSHA: strings.Repeat("c", 40), want: "branch exists without its repository secret"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			harness := newAPIHarness(t)
			harness.secretExists = test.secretExists
			harness.branchSHA = test.branchSHA
			server, client := harness.serverAndClient(t)
			defer server.Close()

			_, err := Bootstrap(context.Background(), client, testOptions(t))
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Bootstrap() error = %v, want containing %q", err, test.want)
			}
			harness.mu.Lock()
			defer harness.mu.Unlock()
			if harness.mutationCount != 0 {
				t.Fatalf("partial-state bootstrap made %d mutations", harness.mutationCount)
			}
		})
	}
}

func TestBootstrapRefusesSecretThatAppearsAfterBranchCreation(t *testing.T) {
	harness := newAPIHarness(t)
	harness.secretSequence = []bool{false, true}
	server, client := harness.serverAndClient(t)
	defer server.Close()

	_, err := Bootstrap(context.Background(), client, testOptions(t))
	if err == nil || !strings.Contains(err.Error(), "appeared concurrently") {
		t.Fatalf("Bootstrap() error = %v, want concurrent-secret refusal", err)
	}
	harness.mu.Lock()
	defer harness.mu.Unlock()
	if harness.secretPutCount != 0 {
		t.Fatalf("bootstrap overwrote a concurrently created secret %d times", harness.secretPutCount)
	}
	if harness.branchSHA != testCommitSHA {
		t.Fatalf("branch SHA = %q, want fail-closed partial branch %q", harness.branchSHA, testCommitSHA)
	}
}

func TestBootstrapRefusesInvalidTargetBeforeGitHubRequest(t *testing.T) {
	harness := newAPIHarness(t)
	server, client := harness.serverAndClient(t)
	defer server.Close()

	opts := testOptions(t)
	opts.StateBranch = "main"
	_, err := Bootstrap(context.Background(), client, opts)
	if err == nil || !strings.Contains(err.Error(), "hive/state") {
		t.Fatalf("Bootstrap() error = %v", err)
	}
	harness.mu.Lock()
	defer harness.mu.Unlock()
	if len(harness.events) != 0 {
		t.Fatalf("invalid options reached GitHub: %v", harness.events)
	}
}

func TestSealForGitHubUsesLibsodiumCompatibleAnonymousBox(t *testing.T) {
	publicKey, privateKey, err := box.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	plaintext := []byte("0123456789abcdef0123456789abcdef")
	encoded, err := sealForGitHub(base64.StdEncoding.EncodeToString(publicKey[:]), plaintext, rand.Reader)
	if err != nil {
		t.Fatalf("sealForGitHub: %v", err)
	}
	sealed, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatalf("decode sealed value: %v", err)
	}
	opened, ok := box.OpenAnonymous(nil, sealed, publicKey, privateKey)
	if !ok || string(opened) != string(plaintext) {
		t.Fatal("GitHub sealed-box value did not decrypt to the original plaintext")
	}
}

func testOptions(t *testing.T) Options {
	t.Helper()
	root := t.TempDir()
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatalf("chmod root: %v", err)
	}
	if err := os.Mkdir(filepath.Join(root, "lifecycle"), 0o700); err != nil {
		t.Fatalf("mkdir lifecycle: %v", err)
	}
	path := filepath.Join(root, "lifecycle", "state.json")
	if err := os.WriteFile(path, []byte(`{"phase":"initialized"}`), 0o600); err != nil {
		t.Fatalf("write prepared state: %v", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatalf("chmod prepared state: %v", err)
	}
	return Options{
		Owner:       "acme",
		Repository:  "widget",
		StateBranch: "hive/state-widget-0123456789ab",
		StateRoot:   root,
		StatePaths:  []string{"lifecycle/state.json"},
		Metadata: hostedstate.Metadata{
			Repository:     hostedstate.RepositoryIdentity{FullName: "acme/widget", ID: 1234},
			StateBranch:    "hive/state-widget-0123456789ab",
			Sequence:       1,
			ExecutionOwner: "bootstrap",
			Release: hostedstate.ReleaseIdentity{
				Version:                    "v0.4.1-integrated.19",
				HiveCommit:                 strings.Repeat("d", 40),
				VisualHiveCommit:           strings.Repeat("e", 40),
				DistributionManifestSHA256: strings.Repeat("6", 64),
				HostedControllerProtocol:   1,
			},
			Controller: hostedstate.ControllerIdentity{
				RunID:          0,
				Attempt:        0,
				Event:          "bootstrap",
				WorkflowPath:   ".github/workflows/hive-controller.yml",
				WorkflowSHA:    strings.Repeat("f", 40),
				DefaultHeadSHA: strings.Repeat("1", 40),
			},
			CreatedAt: time.Date(2026, 7, 15, 20, 0, 0, 0, time.UTC),
		},
	}
}

func bundleForMetadata(t *testing.T, opts Options, metadata hostedstate.Metadata) []byte {
	t.Helper()
	bundle, err := hostedstate.Export(hostedstate.ExportOptions{
		Root:     opts.StateRoot,
		Paths:    append([]string(nil), opts.StatePaths...),
		Metadata: metadata,
		Secret:   []byte("0123456789abcdef0123456789abcdef"),
		Limits:   opts.Limits,
	})
	if err != nil {
		t.Fatalf("export hosted state fixture: %v", err)
	}
	return bundle
}

type errorReader struct{}

func (errorReader) Read([]byte) (int, error) { return 0, errors.New("randomness must not be used") }

type apiHarness struct {
	t                        *testing.T
	mu                       sync.Mutex
	publicKey                *[32]byte
	privateKey               *[32]byte
	secretExists             bool
	secretSequence           []bool
	secretGetCount           int
	branchSHA                string
	branchSequence           []string
	branchGetCount           int
	mutationCount            int
	secretPutCount           int
	deleteRefCount           int
	events                   []string
	stateBundle              []byte
	decryptedSecret          []byte
	commitRequest            []byte
	invalidPublicKey         bool
	secretPutStatus          int
	secretPutPersistsOnError bool
	createReferenceStatus    int
	createRefPersistsOnError bool
}

func newAPIHarness(t *testing.T) *apiHarness {
	t.Helper()
	publicKey, privateKey, err := box.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	return &apiHarness{t: t, publicKey: publicKey, privateKey: privateKey}
}

func (h *apiHarness) serverAndClient(t *testing.T) (*httptest.Server, *github.Client) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(h.serveHTTP))
	client := github.NewClient(nil)
	baseURL := server.URL + "/"
	var err error
	client.BaseURL, err = client.BaseURL.Parse(baseURL)
	if err != nil {
		server.Close()
		t.Fatalf("parse test API URL: %v", err)
	}
	client.UploadURL = client.BaseURL
	return server, client
}

func (h *apiHarness) serveHTTP(response http.ResponseWriter, request *http.Request) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.events = append(h.events, request.Method+" "+request.URL.Path)
	response.Header().Set("Content-Type", "application/json")

	const prefix = "/repos/acme/widget"
	switch {
	case request.Method == http.MethodGet && request.URL.Path == prefix+"/actions/secrets/"+StateSecretName:
		exists := h.secretExists
		if h.secretGetCount < len(h.secretSequence) {
			exists = h.secretSequence[h.secretGetCount]
		}
		h.secretGetCount++
		if !exists {
			h.writeError(response, http.StatusNotFound, "Not Found")
			return
		}
		h.writeJSON(response, http.StatusOK, map[string]any{"name": StateSecretName, "created_at": "2026-07-15T20:00:00Z", "updated_at": "2026-07-15T20:00:00Z"})
	case request.Method == http.MethodGet && request.URL.Path == prefix+"/git/ref/heads/hive/state-widget-0123456789ab":
		sha := h.branchSHA
		if h.branchGetCount < len(h.branchSequence) {
			sha = h.branchSequence[h.branchGetCount]
			h.branchSHA = sha
		}
		h.branchGetCount++
		if sha == "" {
			h.writeError(response, http.StatusNotFound, "Not Found")
			return
		}
		h.writeJSON(response, http.StatusOK, map[string]any{"ref": "refs/heads/hive/state-widget-0123456789ab", "object": map[string]any{"type": "commit", "sha": sha}})
	case request.Method == http.MethodGet && request.URL.Path == prefix+"/contents/"+StateFileName:
		if request.URL.Query().Get("ref") != h.branchSHA {
			h.t.Errorf("state content ref = %q, want exact branch SHA %q", request.URL.Query().Get("ref"), h.branchSHA)
		}
		if len(h.stateBundle) == 0 {
			h.writeError(response, http.StatusNotFound, "Not Found")
			return
		}
		h.writeJSON(response, http.StatusOK, map[string]any{
			"type": "file", "encoding": "base64", "content": base64.StdEncoding.EncodeToString(h.stateBundle),
			"path": StateFileName, "name": StateFileName, "sha": strings.Repeat("2", 40), "size": len(h.stateBundle),
		})
	case request.Method == http.MethodPost && request.URL.Path == prefix+"/git/trees":
		h.recordMutation("create-tree")
		var body struct {
			BaseTree string `json:"base_tree"`
			Tree     []struct {
				Path    string `json:"path"`
				Mode    string `json:"mode"`
				Type    string `json:"type"`
				Content string `json:"content"`
			} `json:"tree"`
		}
		h.decode(request.Body, &body)
		if body.BaseTree != "" || len(body.Tree) != 1 || body.Tree[0].Path != StateFileName || body.Tree[0].Mode != "100644" || body.Tree[0].Type != "blob" {
			h.t.Errorf("invalid orphan tree request: %+v", body)
		}
		h.stateBundle = []byte(body.Tree[0].Content)
		h.writeJSON(response, http.StatusCreated, map[string]any{"sha": testTreeSHA})
	case request.Method == http.MethodPost && request.URL.Path == prefix+"/git/commits":
		h.recordMutation("create-commit")
		h.commitRequest, _ = io.ReadAll(request.Body)
		h.writeJSON(response, http.StatusCreated, map[string]any{"sha": testCommitSHA})
	case request.Method == http.MethodPost && request.URL.Path == prefix+"/git/refs":
		h.recordMutation("create-ref")
		if h.createReferenceStatus != 0 {
			if h.createRefPersistsOnError {
				h.branchSHA = testCommitSHA
			}
			h.writeError(response, h.createReferenceStatus, "reference create failed")
			return
		}
		var body struct {
			Ref string `json:"ref"`
			SHA string `json:"sha"`
		}
		h.decode(request.Body, &body)
		if body.Ref != "refs/heads/hive/state-widget-0123456789ab" || body.SHA != testCommitSHA {
			h.t.Errorf("invalid ref request: %+v", body)
		}
		h.branchSHA = body.SHA
		h.writeJSON(response, http.StatusCreated, map[string]any{"ref": body.Ref, "object": map[string]any{"type": "commit", "sha": body.SHA}})
	case request.Method == http.MethodDelete && request.URL.Path == prefix+"/git/refs/heads/hive/state-widget-0123456789ab":
		h.recordMutation("delete-ref")
		h.deleteRefCount++
		h.branchSHA = ""
		response.WriteHeader(http.StatusNoContent)
	case request.Method == http.MethodGet && request.URL.Path == prefix+"/actions/secrets/public-key":
		key := base64.StdEncoding.EncodeToString(h.publicKey[:])
		if h.invalidPublicKey {
			key = "invalid"
		}
		h.writeJSON(response, http.StatusOK, map[string]any{"key_id": "test-key", "key": key})
	case request.Method == http.MethodPut && request.URL.Path == prefix+"/actions/secrets/"+StateSecretName:
		h.recordMutation("put-secret")
		h.secretPutCount++
		var body struct {
			KeyID          string `json:"key_id"`
			EncryptedValue string `json:"encrypted_value"`
		}
		h.decode(request.Body, &body)
		if body.KeyID != "test-key" {
			h.t.Errorf("secret key ID = %q", body.KeyID)
		}
		sealed, err := base64.StdEncoding.DecodeString(body.EncryptedValue)
		if err != nil {
			h.t.Errorf("decode encrypted secret: %v", err)
		}
		opened, ok := box.OpenAnonymous(nil, sealed, h.publicKey, h.privateKey)
		if !ok {
			h.t.Error("encrypted secret is not a libsodium-compatible sealed box")
		}
		h.decryptedSecret = append([]byte(nil), opened...)
		if h.secretPutStatus != 0 {
			if h.secretPutPersistsOnError {
				h.secretExists = true
			}
			h.writeError(response, h.secretPutStatus, "secret create failed")
			return
		}
		h.secretExists = true
		response.WriteHeader(http.StatusCreated)
	default:
		h.t.Errorf("unexpected API request: %s %s", request.Method, request.URL.Path)
		h.writeError(response, http.StatusNotFound, "unexpected request")
	}
}

func (h *apiHarness) recordMutation(event string) {
	h.mutationCount++
	h.events = append(h.events, event)
}

func (h *apiHarness) decode(reader io.Reader, value any) {
	if err := json.NewDecoder(reader).Decode(value); err != nil {
		h.t.Errorf("decode request: %v", err)
	}
}

func (h *apiHarness) writeJSON(response http.ResponseWriter, status int, value any) {
	response.WriteHeader(status)
	if err := json.NewEncoder(response).Encode(value); err != nil {
		h.t.Errorf("encode response: %v", err)
	}
}

func (h *apiHarness) writeError(response http.ResponseWriter, status int, message string) {
	h.writeJSON(response, status, map[string]any{"message": message})
}

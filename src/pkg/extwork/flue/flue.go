// Package flue binds Hive's external-execution contract (pkg/extwork) to a
// Flue runtime's native surface: keyed admission, incarnation uid, status,
// abort, and per-submission artifacts (Flue commit
// c5a2a725fe1d93209ed294cca90af97060f6f2e2; see #8361 and the Gate 0 record in
// src/docs/design/external-workflow-admission.md).
//
// The adapter talks only to its configured endpoint. Every request is built
// from that base URL plus a validated relative path; there is no method that
// accepts a URL from the engine or from a receipt. It never carries a GitHub
// credential, a dashboard token, or a publication tool, and it honours no
// proxy environment so its traffic cannot be redirected by the process
// environment.
//
// This package is linked into the hive binary only under the extwork_flue
// build tag (cmd/hive/extwork_flue.go); pkg/extwork's guard test enforces that.
package flue

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/extwork"
)

const (
	// Engine is the registry name.
	Engine = "flue"
	// Capability is the contributor-protocol token a peer must declare to be
	// offered Flue-bound work. A peer that opts in without it is refused.
	Capability = "ext-exec/flue"
	// PinnedFlueCommit is the Flue revision this binding was probed against.
	PinnedFlueCommit = "c5a2a725fe1d93209ed294cca90af97060f6f2e2"

	// Settings keys the registry factory reads.
	SettingEndpoint        = extwork.SettingEndpoint
	SettingWorkflowVersion = extwork.SettingWorkflowVersion

	defaultHTTPTimeout = 10 * time.Second
	maxErrorBodyBytes  = 4096
	conflictType       = "submission_conflict"
)

// Config configures an Adapter.
type Config struct {
	// Endpoint is the Flue runtime base URL (http or https, host required,
	// no user info, query, or fragment).
	Endpoint string
	// WorkflowVersion is the engine version the runtime must advertise; a
	// different version is refused, never downgraded.
	WorkflowVersion string
	// HTTPClient overrides the default proxy-free client (tests).
	HTTPClient *http.Client
}

// Adapter implements extwork.Adapter against a Flue runtime.
type Adapter struct {
	base    *url.URL
	version string
	client  *http.Client
}

// New validates the configuration and builds an adapter.
func New(cfg Config) (*Adapter, error) {
	base, err := url.Parse(strings.TrimSpace(cfg.Endpoint))
	if err != nil {
		return nil, fmt.Errorf("flue endpoint: %w", err)
	}
	if (base.Scheme != "http" && base.Scheme != "https") || base.Host == "" || base.User != nil || base.RawQuery != "" || base.Fragment != "" {
		return nil, errors.New("flue endpoint must be an http(s) URL with a host and no user info, query, or fragment")
	}
	if strings.TrimSpace(cfg.WorkflowVersion) == "" {
		return nil, errors.New("flue workflow version is required")
	}
	client := cfg.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: defaultHTTPTimeout, Transport: &http.Transport{Proxy: nil}}
	}
	return &Adapter{base: base, version: strings.TrimSpace(cfg.WorkflowVersion), client: client}, nil
}

// Factory is the extwork.Registry constructor.
func Factory(settings map[string]string) (extwork.Adapter, error) {
	return New(Config{Endpoint: settings[SettingEndpoint], WorkflowVersion: settings[SettingWorkflowVersion]})
}

// Engine returns "flue".
func (a *Adapter) Engine() string { return Engine }

// WorkflowVersion returns the pinned version.
func (a *Adapter) WorkflowVersion() string { return a.version }

// Incarnation implements extwork.Pinner: the runtime uid advertised by the
// engine, recorded in the admission before dispatch.
func (a *Adapter) Incarnation(ctx context.Context) (string, error) {
	var runtime info
	if err := a.do(ctx, http.MethodGet, a.endpoint("/"), nil, &runtime); err != nil {
		return "", err
	}
	if runtime.Incarnation == "" {
		return "", fmt.Errorf("%w: runtime advertises no incarnation", extwork.ErrRefused)
	}
	return runtime.Incarnation, nil
}

func (a *Adapter) endpoint(parts ...string) string {
	u := a.base.JoinPath(parts...)
	return u.String()
}

// engineError is a non-2xx reply.
type engineError struct {
	status int
	typ    string
	body   string
}

func (e *engineError) Error() string {
	return fmt.Sprintf("flue replied %d %s: %s", e.status, e.typ, e.body)
}

func (a *Adapter) do(ctx context.Context, method, rawURL string, body any, out any) error {
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, rawURL, reader)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := a.client.Do(req)
	if err != nil {
		return fmt.Errorf("%w: %v", extwork.ErrTransport, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBodyBytes))
		var typed struct {
			Type string `json:"type"`
		}
		_ = json.Unmarshal(raw, &typed)
		return &engineError{status: resp.StatusCode, typ: typed.Type, body: strings.TrimSpace(string(raw))}
	}
	if out == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("%w: decode: %v", extwork.ErrTransport, err)
	}
	return nil
}

type info struct {
	Engine      string `json:"engine"`
	Version     string `json:"version"`
	Incarnation string `json:"incarnation"`
}

type submissionView struct {
	ID          string              `json:"id"`
	UID         string              `json:"uid"`
	State       string              `json:"state"`
	Stage       string              `json:"stage"`
	ResultClass string              `json:"result_class"`
	Receipt     *extwork.ReceiptRef `json:"receipt"`
}

// Start admits the keyed request. It refuses a request whose authority does
// not carry Capability, whose engine is not flue, or whose runtime advertises
// a different version, and maps submission_conflict to extwork.ErrConflict.
func (a *Adapter) Start(ctx context.Context, req extwork.StartRequest) (extwork.StartResult, error) {
	adm := req.Admission
	if err := adm.Validate(); err != nil {
		return extwork.StartResult{}, err
	}
	if adm.Authority.Capability != Capability {
		return extwork.StartResult{}, fmt.Errorf("%w: authority capability %q is not %s", extwork.ErrRefused, adm.Authority.Capability, Capability)
	}
	if adm.Engine != Engine {
		return extwork.StartResult{}, fmt.Errorf("%w: admission engine %q is not %s", extwork.ErrRefused, adm.Engine, Engine)
	}
	if adm.WorkflowVersion != a.version {
		return extwork.StartResult{}, fmt.Errorf("%w: admission workflow version %q is not the pinned %q", extwork.ErrRefused, adm.WorkflowVersion, a.version)
	}
	if adm.RequestDigest != extwork.RequestDigest(req.Payload) {
		return extwork.StartResult{}, extwork.ErrPayloadDigest
	}
	var runtime info
	if err := a.do(ctx, http.MethodGet, a.endpoint("/"), nil, &runtime); err != nil {
		return extwork.StartResult{}, err
	}
	if runtime.Engine != Engine || runtime.Version != a.version {
		return extwork.StartResult{}, fmt.Errorf("%w: runtime advertises %s %q, pinned %q", extwork.ErrRefused, runtime.Engine, runtime.Version, a.version)
	}
	var reply struct {
		SubmissionID string `json:"submission_id"`
		UID          string `json:"uid"`
		Deduplicated bool   `json:"deduplicated"`
	}
	body := map[string]any{"idempotency_key": string(adm.ExecutionKey()), "payload": req.Payload}
	if err := a.do(ctx, http.MethodPost, a.endpoint("dispatch"), body, &reply); err != nil {
		var ee *engineError
		if errors.As(err, &ee) {
			if ee.status == http.StatusConflict || ee.typ == conflictType {
				return extwork.StartResult{}, fmt.Errorf("%w: %v", extwork.ErrConflict, err)
			}
			return extwork.StartResult{}, fmt.Errorf("%w: %v", extwork.ErrRefused, err)
		}
		return extwork.StartResult{}, err
	}
	if reply.SubmissionID == "" || reply.UID == "" {
		return extwork.StartResult{}, fmt.Errorf("%w: dispatch reply lacks submission id or uid", extwork.ErrRefused)
	}
	return extwork.StartResult{RemoteRunID: reply.SubmissionID, RemoteIncarnation: reply.UID, Deduplicated: reply.Deduplicated}, nil
}

func (a *Adapter) lookup(ctx context.Context, key extwork.ExecutionKey, incarnation string) (submissionView, error) {
	if incarnation != "" {
		// The instance Hive bound must still be the one answering. A
		// deleted-and-recreated runtime answers with a different uid, and
		// whatever it holds (or does not hold) under this key is unrelated.
		live, err := a.Incarnation(ctx)
		if err != nil {
			return submissionView{}, err
		}
		if live != incarnation {
			return submissionView{}, fmt.Errorf("%w: runtime uid %q, pinned %q", extwork.ErrIncarnationMismatch, live, incarnation)
		}
	}
	u := a.base.JoinPath("submissions")
	q := u.Query()
	q.Set("key", string(key))
	u.RawQuery = q.Encode()
	var view submissionView
	if err := a.do(ctx, http.MethodGet, u.String(), nil, &view); err != nil {
		var ee *engineError
		if errors.As(err, &ee) && ee.status == http.StatusNotFound {
			return submissionView{}, extwork.ErrNotFound
		}
		if errors.As(err, &ee) {
			return submissionView{}, fmt.Errorf("%w: %v", extwork.ErrTransport, err)
		}
		return submissionView{}, err
	}
	if incarnation != "" && view.UID != incarnation {
		return submissionView{}, fmt.Errorf("%w: run uid %q, pinned %q", extwork.ErrIncarnationMismatch, view.UID, incarnation)
	}
	return view, nil
}

func mapState(state string) extwork.State {
	switch state {
	case "accepted":
		return extwork.StateAccepted
	case "running":
		return extwork.StateRunning
	case "waiting":
		return extwork.StateWaiting
	case "terminal":
		return extwork.StateTerminal
	default:
		return extwork.StateUnknown
	}
}

// Observe reads the native state for the key.
func (a *Adapter) Observe(ctx context.Context, key extwork.ExecutionKey, incarnation string) (extwork.Observation, error) {
	view, err := a.lookup(ctx, key, incarnation)
	if err != nil {
		return extwork.Observation{State: extwork.StateUnknown}, err
	}
	obs := extwork.Observation{
		State:             mapState(view.State),
		RemoteRunID:       view.ID,
		RemoteIncarnation: view.UID,
		Stage:             view.Stage,
		ResultClass:       view.ResultClass,
		Receipt:           view.Receipt,
		ObservedAt:        time.Now(),
	}
	if obs.State == extwork.StateUnknown {
		obs.Detail = "runtime reported state " + view.State
	}
	return obs, nil
}

// Cancel requests an abort and reports exactly what the runtime confirmed.
func (a *Adapter) Cancel(ctx context.Context, key extwork.ExecutionKey, incarnation string) (extwork.CancelFacts, error) {
	view, err := a.lookup(ctx, key, incarnation)
	if err != nil {
		return extwork.CancelFacts{}, err
	}
	var reply struct {
		Requested    bool   `json:"requested"`
		Acknowledged bool   `json:"acknowledged"`
		Stopped      bool   `json:"stopped"`
		Detail       string `json:"detail"`
	}
	if err := a.do(ctx, http.MethodPost, a.endpoint("submissions", view.ID, "abort"), map[string]string{}, &reply); err != nil {
		// The request left this process but nothing came back that we can
		// trust: it may or may not have been delivered.
		return extwork.CancelFacts{Requested: true, Detail: err.Error()}, err
	}
	return extwork.CancelFacts{Requested: reply.Requested, Acknowledged: reply.Acknowledged, Stopped: reply.Stopped, Detail: reply.Detail}, nil
}

// OpenArtifact streams one artifact of the run by validated relative path.
func (a *Adapter) OpenArtifact(ctx context.Context, key extwork.ExecutionKey, incarnation, path string) (io.ReadCloser, error) {
	clean, err := extwork.CleanArtifactPath(path)
	if err != nil {
		return nil, err
	}
	view, err := a.lookup(ctx, key, incarnation)
	if err != nil {
		return nil, err
	}
	parts := append([]string{"submissions", view.ID, "artifacts"}, strings.Split(clean, "/")...)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.endpoint(parts...), nil)
	if err != nil {
		return nil, err
	}
	resp, err := a.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", extwork.ErrTransport, err)
	}
	if resp.StatusCode == http.StatusNotFound {
		_ = resp.Body.Close()
		return nil, extwork.ErrNotFound
	}
	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		return nil, fmt.Errorf("%w: artifact fetch replied %d", extwork.ErrTransport, resp.StatusCode)
	}
	return resp.Body, nil
}

// Bundle is the bounded, read-only context handed to the engine. It carries
// the admission identities the receipt must echo, a summary, the repository
// name for artifact attribution, optional files, and optional hints. It never
// carries a credential.
type Bundle struct {
	Admission BundleAdmission   `json:"admission"`
	Summary   string            `json:"summary"`
	Repo      string            `json:"repo"`
	Files     map[string]string `json:"files,omitempty"`
	Hints     map[string]string `json:"hints,omitempty"`
}

// BundleAdmission is the identity subset of extwork.Admission the engine sees.
type BundleAdmission struct {
	WorkKey          string `json:"work_key"`
	AssignmentID     string `json:"assignment_id"`
	Generation       uint64 `json:"generation"`
	Stage            string `json:"stage"`
	ContractRevision string `json:"contract_revision"`
	InputRevision    string `json:"input_revision"`
}

// BuildBundle serialises a Bundle deterministically for adm. The caller sets
// adm.RequestDigest to extwork.RequestDigest of the result.
func BuildBundle(adm extwork.Admission, summary, repo string, files, hints map[string]string) ([]byte, error) {
	for k := range files {
		if _, err := extwork.CleanArtifactPath(k); err != nil {
			return nil, fmt.Errorf("bundle file %q: %w", k, err)
		}
	}
	return json.Marshal(Bundle{
		Admission: BundleAdmission{WorkKey: adm.WorkKey, AssignmentID: adm.AssignmentID, Generation: adm.Generation, Stage: adm.Stage, ContractRevision: adm.ContractRevision, InputRevision: adm.InputRevision},
		Summary:   summary,
		Repo:      repo,
		Files:     files,
		Hints:     hints,
	})
}

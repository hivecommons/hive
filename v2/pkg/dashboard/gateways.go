package dashboard

// Gateways CRUD for the dashboard's "Model Gateways" config tab. A hive may
// configure several named, OpenAI-compatible model gateways at once
// (OpenRouter, a LiteLLM proxy, vLLM, llm-d, or a custom endpoint); each agent
// routes through one by naming it as its backend. This file generalizes the
// single-gateway LiteLLM handlers (handleGovernorLiteLLM et al.) into list /
// upsert / delete / test over Governor.Gateways, while the legacy LiteLLM
// routes keep working (a legacy-only hive surfaces its synthesized "litellm"
// gateway via ResolvedGateways()).
//
// SECRETS: a key VALUE typed in the UI is written to an owner-only file on the
// PVC (like the LiteLLM key path) and only the FILE PATH is stored in
// hive.yaml — the key value never enters hive.yaml, logs, or API responses.

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/kubestellar/hive/v2/pkg/config"
	"github.com/kubestellar/hive/v2/pkg/watsonx"
)

// watsonxProbeMintTimeout bounds the IAM token mint done just to run a
// save-time/discover /v1/models probe, so a slow IAM endpoint cannot stall the
// dashboard request. Independent of the probe's own HTTP timeout.
const watsonxProbeMintTimeout = 10 * time.Second

// watsonxEndpointForRegion builds the watsonx model-gateway base URL for a
// region slug, falling back to the default region when blank. Mirrors how the
// UI preset fills the endpoint.
//
// The template itself now lives in pkg/watsonx so the AGENT LAUNCH path can
// resolve the same endpoint for an agent whose backend is "watsonx"; this is a
// thin delegate kept for call-site readability inside the dashboard.
func watsonxEndpointForRegion(region string) string {
	return watsonx.EndpointForRegion(region)
}

// gatewayProbeAuth resolves the bearer + extra request headers a /v1/models
// probe (or model discovery) should present for a gateway. For every kind
// except watsonx this is just the resolved key as the Bearer and no extra
// headers — identical to the prior behavior. For watsonx it mints (and caches)
// an IAM token from the resolved IBM Cloud API key and adds the X-IBM-Project-ID
// header; a mint failure is returned so the probe surfaces a real error instead
// of silently sending the raw key. Never logs the key or token.
func gatewayProbeAuth(kind, key, projectID string) (bearer string, headers map[string]string, err error) {
	if kind != config.GatewayKindWatsonx {
		return key, nil, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), watsonxProbeMintTimeout)
	defer cancel()
	token, err := watsonx.DefaultMinter.Token(ctx, key)
	if err != nil {
		return "", nil, err
	}
	headers = map[string]string{}
	if projectID != "" {
		headers[watsonx.ProjectIDHeader] = projectID
	}
	return token, headers, nil
}

const (
	// openRouterBaseURL is the OpenAI-compatible base URL for OpenRouter. Used
	// as the preset endpoint when the UI picks the OpenRouter gateway kind.
	openRouterBaseURL = "https://openrouter.ai/api/v1"

	// watsonxBaseURLTemplate / watsonxDefaultRegion alias the canonical values
	// in pkg/watsonx. They are NOT redefined here: the agent launch path needs
	// the same template, and two copies is exactly how the gateway half and the
	// agent half of watsonx support were able to disagree.
	watsonxBaseURLTemplate = watsonx.BaseURLTemplate
	watsonxDefaultRegion   = watsonx.DefaultRegion

	// gatewaySecretFileMode / gatewaySecretDirMode keep gateway key files
	// owner-only, matching the LiteLLM key store (litellmKeyFileMode).
	gatewaySecretFileMode = 0o600
	gatewaySecretDirMode  = 0o700

	// maxGatewayNameLen bounds a gateway name so it stays usable as an agent
	// backend id and as a filesystem-safe secret filename component.
	maxGatewayNameLen = 64

	// gatewayKeyNameMaxLen bounds the optional human LABEL for a gateway's key.
	// It is a short display string ("Team inference key"), not the secret, so
	// the limit is generous enough for a descriptive name yet small enough that
	// a stray paste into the name field cannot bloat hive.yaml. Matches the bob
	// key's bobKeyNameMaxLen (#3598).
	gatewayKeyNameMaxLen = 128
)

// validGatewayKinds is the set of kinds the UI presets map to. An empty kind is
// accepted and treated as custom (the endpoint is what actually routes).
var validGatewayKinds = map[string]bool{
	config.GatewayKindOpenRouter: true,
	config.GatewayKindGroq:       true,
	config.GatewayKindLiteLLM:    true,
	config.GatewayKindVLLM:       true,
	config.GatewayKindLLMD:       true,
	config.GatewayKindWatsonx:    true,
	config.GatewayKindCustom:     true,
}

// gatewaySectionResponse builds the safe JSON shape for one gateway. It NEVER
// returns a key value — only the env-var NAME and file PATH already in the
// struct (both are references, safe to expose), plus a hasKey flag and a masked
// tail hint resolved from those references.
func gatewaySectionResponse(gw config.GatewayConfig) map[string]interface{} {
	key := gw.ResolveAPIKey()
	keyHint := ""
	if key != "" {
		keyHint = maskSecretHint(key)
	}
	return map[string]interface{}{
		"name":          gw.Name,
		"kind":          gw.Kind,
		"endpoint":      gw.Endpoint,
		"api_key_env":   gw.APIKeyEnv,
		"api_key_file":  gw.APIKeyFile,
		"default_model": gw.DefaultModel,
		"ca_bundle":     gw.CABundle,
		// watsonx-only identifiers (not secrets) — empty for other kinds and
		// omitted from hive.yaml via omitempty, so existing gateways are
		// unaffected.
		"project_id": gw.ProjectID,
		"region":     gw.Region,
		"hasKey":     key != "",
		"keyHint":    keyHint,
		// keyName is the operator-chosen LABEL for this gateway's key, not the
		// key value — safe to serialize. Empty on gateways that never recorded
		// a name (the dashboard renders that as "(unnamed)"), so no
		// backwards-compat break.
		"keyName": gw.KeyName,
	}
}

// handleGovernorGatewaysList returns the effective gateway list. It uses
// ResolvedGateways() so a legacy-only hive (no `gateways:`, classic `litellm:`
// block) still shows its synthesized "litellm" gateway — nothing is lost when
// the UI replaces the single-LiteLLM tab with the manager.
func (s *Server) handleGovernorGatewaysList(w http.ResponseWriter, r *http.Request) {
	gws := s.deps.Config.Governor.ResolvedGateways()
	out := make([]map[string]interface{}, 0, len(gws))
	for _, gw := range gws {
		out = append(out, gatewaySectionResponse(gw))
	}
	jsonResponse(w, map[string]interface{}{"gateways": out})
}

// handleGovernorGatewaysUpsert creates or replaces a gateway by name. A typed
// key VALUE (api_key) is written to a per-gateway secret file and only the PATH
// is stored — never inlined into hive.yaml. After persisting, the gateway's
// endpoint is registered for model discovery so routing picks it up.
func (s *Server) handleGovernorGatewaysUpsert(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}
	// Optional string fields use pointers so an ABSENT key (the form omits it)
	// is distinguishable from an explicit empty string (clear the field). This
	// lets the UI PUT only the fields it manages without wiping the others on
	// an edit.
	var body struct {
		Name         string  `json:"name"`
		Kind         string  `json:"kind"`
		Endpoint     string  `json:"endpoint"`
		APIKey       string  `json:"api_key"`
		APIKeyEnv    *string `json:"api_key_env"`
		APIKeyFile   *string `json:"api_key_file"`
		DefaultModel *string `json:"default_model"`
		CABundle     *string `json:"ca_bundle"`
		ProjectID    *string `json:"project_id"`
		Region       *string `json:"region"`
		// KeyName is an optional human LABEL recorded alongside the key so
		// managers can tell WHICH key a gateway uses without seeing the value.
		// It is not a secret and is stored in plaintext in hive.yaml. Absent
		// (nil) leaves any existing name untouched; present-but-empty clears it.
		KeyName *string `json:"key_name"`
	}
	if err := decodeBody(r, &body); err != nil {
		jsonError(w, "invalid body", http.StatusBadRequest)
		return
	}

	name := strings.TrimSpace(body.Name)
	if name == "" {
		jsonError(w, "gateway name is required", http.StatusBadRequest)
		return
	}
	if len(name) > maxGatewayNameLen {
		jsonError(w, fmt.Sprintf("gateway name must be at most %d characters", maxGatewayNameLen), http.StatusBadRequest)
		return
	}
	// A gateway name doubles as an agent backend id and as a secret filename
	// component, so keep it to safe characters (no path separators / spaces).
	if strings.ContainsAny(name, " /\\.") {
		jsonError(w, "gateway name may not contain spaces, dots, or slashes", http.StatusBadRequest)
		return
	}

	kind := strings.ToLower(strings.TrimSpace(body.Kind))
	if kind == "" {
		kind = config.GatewayKindCustom
	}
	if !validGatewayKinds[kind] {
		jsonError(w, fmt.Sprintf("invalid gateway kind %q (openrouter, litellm, vllm, llm-d, watsonx, custom)", kind), http.StatusBadRequest)
		return
	}

	endpoint := strings.TrimSpace(body.Endpoint)
	// A watsonx gateway may be configured by REGION alone (the guided form can
	// send a region, not a URL): derive the model-gateway base from the region
	// template so the user never has to hand-type the endpoint.
	if endpoint == "" && kind == config.GatewayKindWatsonx {
		region := ""
		if body.Region != nil {
			region = strings.TrimSpace(*body.Region)
		}
		endpoint = watsonxEndpointForRegion(region)
	}
	if endpoint == "" {
		jsonError(w, "endpoint is required", http.StatusBadRequest)
		return
	}
	if err := validateGatewayEndpoint(endpoint); err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	if isPrivateURL(r.Context(), endpoint) {
		jsonError(w, "gateway endpoint must not point to private/internal addresses", http.StatusBadRequest)
		return
	}

	// Guardrail: an env-var NAME field must never hold a key VALUE (which would
	// bake the key into hive.yaml and resolve empty at runtime — the LiteLLM leak).
	if body.APIKeyEnv != nil {
		keyEnv := strings.TrimSpace(*body.APIKeyEnv)
		if keyEnv != "" && looksLikeAPIKeyValue(keyEnv) {
			jsonError(w, "this looks like an API key — send it as api_key instead; api_key_env takes an environment variable NAME", http.StatusBadRequest)
			return
		}
	}
	if body.APIKeyFile != nil {
		keyFile := strings.TrimSpace(*body.APIKeyFile)
		if keyFile != "" && !strings.HasPrefix(keyFile, "/") && looksLikeAPIKeyValue(keyFile) {
			jsonError(w, "this looks like an API key — send it as api_key instead; api_key_file takes a file PATH", http.StatusBadRequest)
			return
		}
	}

	// Normalize the optional key NAME. It is a label, not a secret: leading and
	// trailing whitespace is trimmed (a name is a display string, not a token),
	// interior whitespace is allowed ("Team inference key"), and only the length
	// is bounded. nil means "leave the existing name as-is"; an explicit empty
	// string clears it. Validated before any secret file is written so a
	// rejected name never leaves a stray key behind.
	var keyName string
	keyNameProvided := body.KeyName != nil
	if keyNameProvided {
		keyName = strings.TrimSpace(*body.KeyName)
		if len(keyName) > gatewayKeyNameMaxLen {
			jsonError(w, fmt.Sprintf("key name is too long (limit %d characters)", gatewayKeyNameMaxLen), http.StatusBadRequest)
			return
		}
	}

	cfg := s.deps.Config

	// Find an existing entry so we can preserve fields the caller left blank
	// (notably a previously-stored key file when no new key is typed).
	gws := append([]config.GatewayConfig(nil), cfg.Governor.Gateways...)
	idx := -1
	for i := range gws {
		if strings.EqualFold(gws[i].Name, name) {
			idx = i
			break
		}
	}

	gw := config.GatewayConfig{Name: name, Kind: kind, Endpoint: endpoint}
	if idx >= 0 {
		// Preserve all prior optional fields; only overwrite the ones the caller
		// actually sent (non-nil pointer). This lets the UI PUT the fields it
		// manages without wiping the rest of an existing gateway.
		gw.APIKeyEnv = gws[idx].APIKeyEnv
		gw.APIKeyFile = gws[idx].APIKeyFile
		gw.DefaultModel = gws[idx].DefaultModel
		gw.CABundle = gws[idx].CABundle
		gw.ProjectID = gws[idx].ProjectID
		gw.Region = gws[idx].Region
		gw.KeyName = gws[idx].KeyName
	}
	if body.APIKeyEnv != nil {
		gw.APIKeyEnv = strings.TrimSpace(*body.APIKeyEnv)
	}
	if body.APIKeyFile != nil {
		gw.APIKeyFile = strings.TrimSpace(*body.APIKeyFile)
	}
	if body.DefaultModel != nil {
		gw.DefaultModel = strings.TrimSpace(*body.DefaultModel)
	}
	if body.CABundle != nil {
		gw.CABundle = strings.TrimSpace(*body.CABundle)
	}
	if body.ProjectID != nil {
		gw.ProjectID = strings.TrimSpace(*body.ProjectID)
	}
	if body.Region != nil {
		gw.Region = strings.TrimSpace(*body.Region)
	}
	// Record the label only when the caller supplied the field, so a name-less
	// "Replace key" PUT keeps the existing name rather than wiping it. A
	// provided empty string intentionally clears it.
	if keyNameProvided {
		gw.KeyName = keyName
	}

	submittedKey := strings.TrimSpace(body.APIKey)

	// watsonx needs two things an ordinary OpenAI-compatible gateway does not:
	// a project id (scopes billing/limits; sent as the X-IBM-Project-ID header)
	// and an IBM Cloud API key (exchanged for a short-lived IAM bearer — the raw
	// key is never sent). Reject a watsonx gateway missing either so the failure
	// is a clear config error now, not an opaque 401/400 at agent inference time.
	// Checked BEFORE the key is written to disk so an invalid watsonx config
	// never leaves a stray secret file behind.
	if kind == config.GatewayKindWatsonx {
		if gw.ProjectID == "" {
			jsonError(w, "watsonx gateway requires a project_id (the watsonx project or space id)", http.StatusBadRequest)
			return
		}
		// A key is present if one was just submitted or a prior reference
		// resolves to a value.
		if submittedKey == "" && gw.ResolveAPIKey() == "" {
			jsonError(w, "watsonx gateway requires an IBM Cloud API key", http.StatusBadRequest)
			return
		}
	}

	// Store a submitted key VALUE outside hive.yaml and point api_key_file at it.
	if submittedKey != "" {
		path, err := s.storeGatewayAPIKey(name, submittedKey)
		if err != nil {
			jsonError(w, "failed to store API key: "+redactSecret(err.Error(), submittedKey), http.StatusInternalServerError)
			return
		}
		gw.APIKeyFile = path
		// The legacy litellm: section keeps a SEPARATE key store
		// (/data/secrets/litellm_api_key + the hive-secrets Secret) that
		// pre-gateways code paths (local proxy, env fallbacks) still resolve.
		// When this gateway is the one the legacy section points at, sync the
		// legacy store too — otherwise a key rotated here leaves the legacy
		// file holding the old (possibly revoked) key, and anything resolving
		// through it fails against the gateway from then on.
		if kind == config.GatewayKindLiteLLM && s.legacyLiteLLMSectionMatches(gw) {
			if legacyPath, syncErr := s.storeLiteLLMAPIKey(submittedKey); syncErr != nil {
				s.logger.Warn("legacy litellm key store not synced after gateway key save",
					"gateway", name, "error", syncErr)
			} else {
				cfg.Governor.LiteLLM.APIKeyFile = legacyPath
			}
		}
	}

	if idx >= 0 {
		gws[idx] = gw
	} else {
		gws = append(gws, gw)
	}
	cfg.Governor.Gateways = gws

	if err := s.saveConfig(); err != nil {
		s.logger.Error("failed to persist config after gateway upsert", "error", err)
	}
	s.auditFromRequest(r, "config_governor_gateway_upsert", auditDetail("gateway", name, "kind", kind), "")

	// Register the endpoint for model discovery so routing/dropdowns pick it up.
	s.registerGatewayEndpoints()
	s.refreshAndPersist()

	// Live save-time probe so a bad endpoint/key fails visibly now, not as
	// agent 401s later. Uses the just-submitted key when present (the key file
	// may not reflect it in the same request on some volumes).
	resp := map[string]interface{}{"ok": true, "status": "updated", "gateway": gatewaySectionResponse(gw)}
	if probe := s.gatewayProbeResult(gw, submittedKey); probe != nil {
		resp["probe"] = probe
	}
	jsonResponse(w, resp)
}

// handleGovernorGatewaysDelete removes a gateway by name. It refuses (409) to
// delete a gateway that an agent currently references as its backend, listing
// the offending agents so the operator can repoint them first.
func (s *Server) handleGovernorGatewaysDelete(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}
	name := strings.TrimSpace(r.PathValue("name"))
	if name == "" {
		jsonError(w, "gateway name is required", http.StatusBadRequest)
		return
	}
	cfg := s.deps.Config

	// Refuse to orphan agents that route through this gateway.
	var inUse []string
	for agentName, agent := range cfg.Agents {
		if strings.EqualFold(agent.Backend, name) {
			inUse = append(inUse, agentName)
		}
	}
	if len(inUse) > 0 {
		jsonError(w, fmt.Sprintf("gateway %q is in use by agent(s): %s — repoint them to another backend first",
			name, strings.Join(inUse, ", ")), http.StatusConflict)
		return
	}

	idx := -1
	for i := range cfg.Governor.Gateways {
		if strings.EqualFold(cfg.Governor.Gateways[i].Name, name) {
			idx = i
			break
		}
	}
	if idx < 0 {
		jsonError(w, "gateway not found: "+name, http.StatusNotFound)
		return
	}
	cfg.Governor.Gateways = append(cfg.Governor.Gateways[:idx], cfg.Governor.Gateways[idx+1:]...)

	if err := s.saveConfig(); err != nil {
		s.logger.Error("failed to persist config after gateway delete", "error", err)
	}
	s.auditFromRequest(r, "config_governor_gateway_delete", auditDetail("gateway", name), "")
	s.registerGatewayEndpoints()
	s.refreshAndPersist()
	okResponse(w, map[string]string{"status": "deleted", "gateway": name})
}

// handleGovernorGatewaysDiscover probes an ARBITRARY endpoint (+ optional key)
// for its /v1/models list so the add/edit form's Default Model dropdown can
// populate before the gateway is saved. The key is used transiently for the
// probe only — it is never stored, logged, or echoed. If no key is supplied,
// the resolved key for a gateway of the same name (if one exists) is used, so
// re-opening an existing gateway populates its models without re-entering the
// key. Returns {ok, models:[...]} or {ok:false, error}.
func (s *Server) handleGovernorGatewaysDiscover(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name      string `json:"name"`
		Endpoint  string `json:"endpoint"`
		APIKey    string `json:"api_key"`
		Kind      string `json:"kind"`
		ProjectID string `json:"project_id"`
	}
	if err := decodeBody(r, &body); err != nil {
		jsonError(w, "invalid body", http.StatusBadRequest)
		return
	}
	endpoint := strings.TrimSpace(body.Endpoint)
	if endpoint == "" {
		jsonError(w, "endpoint is required", http.StatusBadRequest)
		return
	}
	if err := validateGatewayEndpoint(endpoint); err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	if isPrivateURL(r.Context(), endpoint) {
		jsonError(w, "gateway endpoint must not point to private/internal addresses", http.StatusBadRequest)
		return
	}
	key := strings.TrimSpace(body.APIKey)
	kind := strings.ToLower(strings.TrimSpace(body.Kind))
	projectID := strings.TrimSpace(body.ProjectID)
	// For fields the form did not send (editing an existing gateway without
	// retyping the key, or a kind/project the client omitted), fall back to the
	// stored gateway of this name.
	if key == "" || kind == "" || projectID == "" {
		if gw := s.deps.Config.Governor.ResolveGateway(strings.TrimSpace(body.Name)); gw != nil {
			if key == "" {
				key = gw.ResolveAPIKey()
			}
			if kind == "" {
				kind = gw.Kind
			}
			if projectID == "" {
				projectID = gw.ProjectID
			}
		}
	}
	bearer, headers, err := gatewayProbeAuth(kind, key, projectID)
	if err != nil {
		// A watsonx mint failure means the key is missing or invalid — model
		// population must NOT happen until a valid key is entered, so this is
		// an error, never a fallback list (operator requirement: discovery and
		// population only after a valid watsonx.ai API key).
		jsonResponse(w, map[string]interface{}{"ok": false, "error": redactSecret(err.Error(), key)})
		return
	}
	models, err := fetchModelsWithHeaders(endpoint, bearer, headers)
	if err != nil {
		// For watsonx the key has already been VALIDATED (the IAM mint above
		// succeeded); only the model listing failed. Offering the static
		// Granite list here keeps the Default Model dropdown usable without
		// ever populating models for an unvalidated key.
		if kind == config.GatewayKindWatsonx {
			jsonResponse(w, map[string]interface{}{"ok": true, "models": watsonx.GraniteFallbackModels, "fallback": true})
			return
		}
		jsonResponse(w, map[string]interface{}{"ok": false, "error": redactSecret(redactSecret(err.Error(), key), bearer)})
		return
	}
	if len(models) == 0 && kind == config.GatewayKindWatsonx {
		models = watsonx.GraniteFallbackModels
	}
	jsonResponse(w, map[string]interface{}{"ok": true, "models": models})
}

// handleGovernorGatewaysTest runs a live /v1/models probe against a named
// gateway's resolved endpoint + key and returns {ok, model_count, error}.
func (s *Server) handleGovernorGatewaysTest(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimSpace(r.PathValue("name"))
	gw := s.deps.Config.Governor.ResolveGateway(name)
	if gw == nil {
		jsonError(w, "gateway not found: "+name, http.StatusNotFound)
		return
	}
	probe := s.gatewayProbeResult(*gw, "")
	if probe == nil {
		jsonError(w, "gateway has no endpoint configured", http.StatusBadRequest)
		return
	}
	jsonResponse(w, map[string]interface{}{"ok": true, "probe": probe})
}

// gatewayProbeResult runs the shared live /v1/models probe (probeLiteLLMModels)
// against a gateway's endpoint using its resolved key, unless overrideKey is set
// (a key submitted in the same request the key files may not reflect yet).
// Returns {ok:true, model_count:N} or {ok:false, error:...}, or nil when the
// gateway has no endpoint.
func (s *Server) gatewayProbeResult(gw config.GatewayConfig, overrideKey string) map[string]interface{} {
	ep := strings.TrimSpace(gw.Endpoint)
	if ep == "" {
		return nil
	}
	probeKey := overrideKey
	if probeKey == "" {
		probeKey = gw.ResolveAPIKey()
	}
	bearer, headers, err := gatewayProbeAuth(gw.Kind, probeKey, gw.ProjectID)
	if err != nil {
		return map[string]interface{}{"ok": false, "error": redactSecret(err.Error(), probeKey)}
	}
	n, err := probeModelsWithHeaders(ep, bearer, headers)
	if err != nil {
		// Redact both the raw key and the minted bearer (watsonx) from any
		// echoed upstream error body.
		return map[string]interface{}{"ok": false, "error": redactSecret(redactSecret(err.Error(), probeKey), bearer)}
	}
	return map[string]interface{}{"ok": true, "model_count": n}
}

// registerGatewayEndpoints (re-)registers every resolved gateway's endpoint for
// model discovery, keyed by gateway name, so the UI's per-gateway /v1/models
// dropdown and any per-gateway routing can resolve an endpoint. It always
// includes the legacy "litellm" registration too (ResolvedGateways synthesizes
// it), so the classic single-gateway path keeps working unchanged.
func (s *Server) registerGatewayEndpoints() {
	for _, gw := range s.deps.Config.Governor.ResolvedGateways() {
		ep := strings.TrimSpace(gw.Endpoint)
		if ep == "" {
			s.UpdateInferenceEndpoint(gw.Name, nil)
			continue
		}
		s.UpdateInferenceEndpoint(gw.Name, []string{ep})
	}
}

// validateGatewayEndpoint checks the endpoint parses as an absolute http(s)
// URL, mirroring LiteLLMConfig.Validate.
func validateGatewayEndpoint(endpoint string) error {
	u, err := url.Parse(endpoint)
	if err != nil {
		return fmt.Errorf("endpoint %q is not a valid URL: %w", endpoint, err)
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("endpoint %q must be an absolute http(s) URL", endpoint)
	}
	return nil
}

// gatewaySecretsDir is where per-gateway key VALUES are written (a package var
// so tests can point it at a temp dir). Production value never changes at
// runtime. Sits under the same PVC-backed writable secrets dir as the LiteLLM
// key so it survives pod restarts and hosted users can set keys without cluster
// access.
var gatewaySecretsDir = config.WritableSecretsDir

// storeGatewayAPIKey writes a gateway's key VALUE to an owner-only file on the
// PVC and returns the file PATH to record in hive.yaml (api_key_file). The key
// value is never logged, echoed, or written to hive.yaml — only the path is.
// The filename is derived from the gateway name (already validated to safe
// characters) so multiple gateways keep independent keys.
func (s *Server) storeGatewayAPIKey(name, key string) (string, error) {
	if err := os.MkdirAll(gatewaySecretsDir, gatewaySecretDirMode); err != nil {
		return "", actionableWriteError(gatewaySecretsDir, err)
	}
	path := filepath.Join(gatewaySecretsDir, "gateway_"+strings.ToLower(name)+"_api_key")
	if err := os.WriteFile(path, []byte(key), gatewaySecretFileMode); err != nil {
		return "", actionableWriteError(path, err)
	}
	// WriteFile does not change the mode of a pre-existing file.
	if err := os.Chmod(path, gatewaySecretFileMode); err != nil {
		return "", actionableWriteError(path, err)
	}
	s.logger.Info("gateway api key stored", "gateway", name, "api_key_file", path)
	return path, nil
}

// legacyLiteLLMSectionMatches reports whether the legacy governor.litellm
// section refers to the same proxy as gw, meaning its key store must be kept
// in sync when gw's key rotates. Matching is by endpoint; an unset legacy
// endpoint falls back to the conventional gateway name "litellm" (the
// built-in method's gateway shares its name with its kind).
func (s *Server) legacyLiteLLMSectionMatches(gw config.GatewayConfig) bool {
	if s.deps == nil || s.deps.Config == nil {
		return false
	}
	legacy := strings.TrimRight(strings.TrimSpace(s.deps.Config.Governor.LiteLLM.Endpoint), "/")
	if legacy == "" {
		return strings.EqualFold(gw.Name, config.GatewayKindLiteLLM)
	}
	return strings.EqualFold(legacy, strings.TrimRight(strings.TrimSpace(gw.Endpoint), "/"))
}

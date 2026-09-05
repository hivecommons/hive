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
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/watsonx"
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

func (s *Server) gatewaySectionResponse(gw config.GatewayConfig) map[string]interface{} {
	resp := gatewaySectionResponse(gw)
	for _, st := range s.GatewayHealthState() {
		if strings.EqualFold(st.Name, gw.Name) {
			resp["health"] = st
			break
		}
	}
	return resp
}

// handleGovernorGatewaysList returns the effective gateway list. It uses
// ResolvedGateways() so a legacy-only hive (no `gateways:`, classic `litellm:`
// block) still shows its synthesized "litellm" gateway — nothing is lost when
// the UI replaces the single-LiteLLM tab with the manager.
func (s *Server) handleGovernorGatewaysList(w http.ResponseWriter, r *http.Request) {
	gws := s.deps.Config.Governor.ResolvedGateways()
	out := make([]map[string]interface{}, 0, len(gws))
	for _, gw := range gws {
		out = append(out, s.gatewaySectionResponse(gw))
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
		// SECURITY (audit N8, CWE-200/918): reject an out-of-root path at the
		// door with a clear 400, rather than storing it and letting it resolve
		// to "" later (which would look like a mysteriously broken gateway).
		//
		// Note the guardrail ABOVE only fires for a RELATIVE key-looking string —
		// `!strings.HasPrefix(keyFile, "/")` short-circuits it for every absolute
		// path, which is precisely why any absolute path used to be accepted and
		// stored verbatim. ResolveAPIKey enforces the same confinement at read
		// time for paths that reach hive.yaml by other routes.
		if keyFile != "" && !config.SecretFilePathAllowed(keyFile) {
			jsonError(w, "api_key_file must be under /secrets or "+config.WritableSecretsDir, http.StatusBadRequest)
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
	resp := map[string]interface{}{"ok": true, "status": "updated", "gateway": s.gatewaySectionResponse(gw)}
	// SECURITY (audit F6, CWE-200): probe with the SUBMITTED key only, never a
	// stored one.
	//
	// This handler preserves a previously-stored api_key_file when the caller
	// omits a key (intended — see TestGatewaysUpsert_PreservesKeyOnEdit) while
	// taking the endpoint from the request body. Probing with the resolved key
	// chained those into an exfiltration primitive: one PUT naming an existing
	// gateway, supplying only a new endpoint, walked the stored credential to a
	// server the caller controls. The caller never had to know the key.
	//
	// Blanket private-IP denial is the wrong fix — in-cluster gateways are
	// legitimate and it would break them. The invariant that actually holds is
	// narrower: a credential the caller did not supply must never be sent to an
	// endpoint the caller did. When the caller submits the key, they already
	// possess it, so probing their endpoint discloses nothing new.
	if submittedKey != "" {
		if probe := s.gatewayProbeResult(gw, submittedKey); probe != nil {
			resp["probe"] = probe
		}
	} else {
		// No new key: the endpoint may have changed, so a stored credential is
		// not ours to send. Verifying it stays possible via the separate
		// /gateways/{name}/test route, which probes a gateway's own PERSISTED
		// endpoint rather than one supplied in this request.
		resp["probe"] = map[string]interface{}{
			"ok":      false,
			"skipped": "no key submitted — endpoint not probed with the stored credential; use the gateway test action to verify",
		}
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
	// SECURITY (audit F13): discover mutates nothing, but it drives an outbound
	// request to a caller-named endpoint and can attach a stored credential, so
	// it is gated like the other gateway routes rather than left open.
	if !requireOwnerRole(w, r) {
		return
	}

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
	if err := guardDiscoverEndpoint(r.Context(), endpoint); err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
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
			// SECURITY (audit F13, CWE-200): this is the same defect as N8 and F6,
			// which were fixed on the UPSERT handler but never here. A credential
			// the caller did not supply must never be sent to an endpoint the
			// caller chose. Falling back to the stored key while taking the
			// endpoint from the request body made one POST — naming an existing
			// gateway and any attacker-controlled endpoint — walk the stored key
			// straight out. The caller never had to know the key.
			//
			// The stored key is therefore usable only when the caller names the
			// gateway's OWN persisted endpoint: that address already receives this
			// key on every agent request, so probing it discloses nothing new. Any
			// other endpoint requires the caller to supply their own key, which
			// they already possess.
			//
			// kind/project_id are NOT secrets, so they still fall back freely —
			// only the key is endpoint-bound.
			if key == "" && sameGatewayEndpoint(endpoint, gw.Endpoint) {
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
		msg := redactSecret(err.Error(), probeKey)
		if store := s.gatewayHealthStore(); store != nil {
			store.RecordError(gw.Name, errors.New(msg), time.Now())
		}
		return map[string]interface{}{"ok": false, "error": msg}
	}
	n, err := probeModelsWithHeaders(ep, bearer, headers)
	if err != nil {
		// Redact both the raw key and the minted bearer (watsonx) from any
		// echoed upstream error body before the message is stored or returned.
		msg := redactSecret(redactSecret(err.Error(), probeKey), bearer)
		if store := s.gatewayHealthStore(); store != nil {
			store.RecordError(gw.Name, errors.New(msg), time.Now())
		}
		return map[string]interface{}{"ok": false, "error": msg}
	}
	if store := s.gatewayHealthStore(); store != nil {
		store.Clear(gw.Name)
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
	_, err := parseGatewayEndpoint(endpoint)
	return err
}

// parseGatewayEndpoint is validateGatewayEndpoint's shared parse step, returning
// the parsed URL so callers that need the host do not re-parse (and cannot drift
// from what validation accepted).
func parseGatewayEndpoint(endpoint string) (*url.URL, error) {
	u, err := url.Parse(endpoint)
	if err != nil {
		return nil, fmt.Errorf("endpoint %q is not a valid URL: %w", endpoint, err)
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("endpoint %q must be an absolute http(s) URL", endpoint)
	}
	return u, nil
}

// sameGatewayEndpoint reports whether a caller-supplied endpoint addresses the
// SAME upstream as a gateway's persisted endpoint. It is the gate on reusing a
// stored credential (audit F13), so it compares canonically and is deliberately
// strict: anything it cannot prove identical returns false, which merely costs
// the caller a re-typed key.
//
// Canonicalisation covers the ways two spellings of one address differ
// innocently — scheme/host case, the implicit :80/:443 port, a trailing slash —
// and nothing else. In particular userinfo must match, so
// "https://evil@gw.example" is NOT the same upstream as "https://gw.example":
// Go sends userinfo as Basic credentials and the host itself can differ under
// some parsers, both of which are exactly the confusion an attacker wants.
func sameGatewayEndpoint(supplied, stored string) bool {
	a, errA := parseGatewayEndpoint(strings.TrimSpace(supplied))
	b, errB := parseGatewayEndpoint(strings.TrimSpace(stored))
	if errA != nil || errB != nil {
		return false
	}
	return canonicalGatewayEndpoint(a) == canonicalGatewayEndpoint(b)
}

// canonicalGatewayEndpoint renders a URL in a normalized form for equality
// comparison: lowercased scheme and host, the default port made explicit, and a
// trailing slash trimmed from the path.
func canonicalGatewayEndpoint(u *url.URL) string {
	scheme := strings.ToLower(u.Scheme)
	host := strings.ToLower(u.Hostname())
	port := u.Port()
	if port == "" {
		if scheme == "https" {
			port = "443"
		} else {
			port = "80"
		}
	}
	user := ""
	if u.User != nil {
		user = u.User.String() + "@"
	}
	path := strings.TrimRight(u.Path, "/")
	return scheme + "://" + user + net.JoinHostPort(host, port) + path
}

// errGatewayEndpointPrivate is returned when a caller-supplied discover target
// resolves into private/loopback/link-local space.
var errGatewayEndpointPrivate = errors.New("endpoint resolves to a private, loopback or link-local address and cannot be probed from the discover form; save the gateway and use its test action instead")

// guardDiscoverEndpoint is the SSRF gate on the DISCOVER path (audit F13).
//
// Scope note, because the placement is deliberate and easy to "fix" wrongly:
// this check is NOT in validateGatewayEndpoint. That helper also validates the
// UPSERT path, where in-cluster gateways are a supported, documented
// configuration — the bundled local litellm proxy is http://127.0.0.1:18445 and
// explicit vllm/llm-d endpoints may use *.svc.cluster.local (see HIVE_VLLM_ENDPOINT). Blanket
// private-address denial there would break real deployments, which is why the
// F6 fix explicitly rejected it as the remedy.
//
// Discover is different: it is an unauthenticated-shaped, caller-driven fetch
// whose only job is populating a dropdown. Refusing to aim it at internal
// addresses removes the SSRF probe primitive (cloud metadata at 169.254.169.254,
// port-scanning localhost via error/timing differences) while leaving every
// legitimate in-cluster gateway configurable through upsert + the test action,
// which probe a PERSISTED endpoint an owner already committed to.
//
// Resolution is done via DNS, not string matching, so "evil.example" pointing at
// 169.254.169.254 is caught; it fails CLOSED on resolution error.
func guardDiscoverEndpoint(ctx context.Context, endpoint string) error {
	u, err := parseGatewayEndpoint(endpoint)
	if err != nil {
		return err
	}
	if isPrivateURL(ctx, u.Scheme+"://"+u.Host) {
		return errGatewayEndpointPrivate
	}
	return nil
}

// gatewaySecretsDir is where per-gateway key VALUES are written (a package var
// so tests can point it at a temp dir). Production value never changes at
// runtime. Sits under the same PVC-backed writable secrets dir as the LiteLLM
// key so it survives pod restarts and hosted users can set keys without cluster
// access.
var gatewaySecretsDir = config.WritableSecretsDir

// setGatewaySecretsDirForTest repoints where gateway key VALUES are written AND
// registers that directory with config's secret-file read gate (audit N8), then
// returns a restore func.
//
// The two must move together. The read gate confines api_key_file to the managed
// secrets dirs; a test that repoints only the write var would store a key it can
// never read back, and "just disable the gate in tests" would mean the
// confinement is never actually exercised.
func setGatewaySecretsDirForTest(dir string) func() {
	prevDir := gatewaySecretsDir
	gatewaySecretsDir = dir
	restoreRoot := config.AllowSecretFileRoot(dir)
	return func() {
		restoreRoot()
		gatewaySecretsDir = prevDir
	}
}

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

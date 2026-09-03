package dashboard

// Inference-auth settings (governor.vllm / governor.llm-d) for the Model
// Gateways config tab (#4217). LiteLLM auth is already exposed there; this
// closes the asymmetry for the self-hosted backends. Only key REFERENCES are
// accepted and returned — a header NAME, an env var NAME, a file PATH, and an
// optional endpoint override. The key VALUE never enters hive.yaml, logs, or
// API responses.

import (
	"net/http"
	"strings"

	"github.com/hivecommons/hive/pkg/config"
)

// inferenceAuthSectionResponse builds the safe JSON shape for one backend's
// InferenceAuthConfig. Everything in the struct is a reference (names/paths),
// so it is safe to serialize verbatim.
func inferenceAuthSectionResponse(c config.InferenceAuthConfig) map[string]interface{} {
	return map[string]interface{}{
		"api_key_header": c.APIKeyHeader,
		"api_key_env":    c.APIKeyEnv,
		"api_key_file":   c.APIKeyFile,
		"endpoint":       c.Endpoint,
	}
}

// handleGovernorInferenceAuthGet returns the vllm/llm-d discovery-auth
// configuration. References only — never a key value.
func (s *Server) handleGovernorInferenceAuthGet(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}
	gov := &s.deps.Config.Governor
	jsonResponse(w, map[string]interface{}{
		"vllm": inferenceAuthSectionResponse(gov.VLLM),
		"llmd": inferenceAuthSectionResponse(gov.LLMD),
	})
}

// inferenceAuthBody is one backend's section of the PUT body. Pointer fields
// distinguish "absent — leave unchanged" from "empty — clear it", the same
// contract as handleGovernorLiteLLM.
type inferenceAuthBody struct {
	APIKeyHeader *string `json:"api_key_header"`
	APIKeyEnv    *string `json:"api_key_env"`
	APIKeyFile   *string `json:"api_key_file"`
	Endpoint     *string `json:"endpoint"`
}

// applyInferenceAuthBody validates one backend's submitted fields against a
// copy of its current config and returns the updated copy, or a non-empty
// error message. Guardrails mirror handleGovernorLiteLLM: an env/file NAME
// field must never hold a key VALUE, and a key file path must stay inside the
// managed secrets dirs.
func applyInferenceAuthBody(cur config.InferenceAuthConfig, body *inferenceAuthBody) (config.InferenceAuthConfig, string) {
	if body == nil {
		return cur, ""
	}
	if body.APIKeyHeader != nil {
		cur.APIKeyHeader = strings.TrimSpace(*body.APIKeyHeader)
	}
	if body.APIKeyEnv != nil {
		keyEnv := strings.TrimSpace(*body.APIKeyEnv)
		if looksLikeAPIKeyValue(keyEnv) {
			return cur, "this looks like an API key — api_key_env takes an environment variable NAME (e.g. HIVE_VLLM_API_KEY)"
		}
		cur.APIKeyEnv = keyEnv
	}
	if body.APIKeyFile != nil {
		keyFile := strings.TrimSpace(*body.APIKeyFile)
		if !strings.HasPrefix(keyFile, "/") && looksLikeAPIKeyValue(keyFile) {
			return cur, "this looks like an API key — api_key_file takes a file PATH (e.g. /secrets/vllm_api_key)"
		}
		// SECURITY (audit N8, CWE-200/918): same confinement as the gateway
		// and LiteLLM handlers — reject an out-of-root path at the door.
		if keyFile != "" && !config.SecretFilePathAllowed(keyFile) {
			return cur, "api_key_file must be under /secrets or " + config.WritableSecretsDir
		}
		cur.APIKeyFile = keyFile
	}
	if body.Endpoint != nil {
		endpoint := strings.TrimSpace(*body.Endpoint)
		if endpoint != "" {
			if err := validateGatewayEndpoint(endpoint); err != nil {
				return cur, err.Error()
			}
		}
		cur.Endpoint = endpoint
	}
	return cur, ""
}

// handleGovernorInferenceAuthPut updates governor.vllm / governor.llm-d from
// the Model Gateways tab. Each backend section is optional; within a section,
// absent fields are left untouched. Persists via the same secret-free overlay
// path as every other governor-config writer.
func (s *Server) handleGovernorInferenceAuthPut(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}
	var body struct {
		VLLM *inferenceAuthBody `json:"vllm"`
		LLMD *inferenceAuthBody `json:"llmd"`
	}
	if err := decodeBody(r, &body); err != nil {
		jsonError(w, "invalid body", http.StatusBadRequest)
		return
	}
	gov := &s.deps.Config.Governor
	// Validate both sections against copies before mutating either, so a bad
	// llm-d field cannot leave a half-applied vllm change behind.
	vllm, msg := applyInferenceAuthBody(gov.VLLM, body.VLLM)
	if msg != "" {
		jsonError(w, "vllm: "+msg, http.StatusBadRequest)
		return
	}
	llmd, msg := applyInferenceAuthBody(gov.LLMD, body.LLMD)
	if msg != "" {
		jsonError(w, "llm-d: "+msg, http.StatusBadRequest)
		return
	}
	gov.VLLM = vllm
	gov.LLMD = llmd

	if err := s.saveConfig(); err != nil {
		s.logger.Error("failed to persist config after inference-auth update", "error", err)
	}
	s.auditFromRequest(r, "config_governor_inference_auth", auditDetail("section", "inference-auth"), "")
	s.refreshAndPersist()

	jsonResponse(w, map[string]interface{}{
		"ok":     true,
		"status": "updated",
		"vllm":   inferenceAuthSectionResponse(gov.VLLM),
		"llmd":   inferenceAuthSectionResponse(gov.LLMD),
	})
}

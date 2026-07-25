package dashboard

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"strings"

	"github.com/kubestellar/hive/v2/pkg/github"
)

var integratedSetupCommitPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)
var integratedSetupDigestPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// IntegratedSetupRequest is the bounded dashboard control-plane contract for
// installing Visual Hive into the repository already owned by this Hive.
// Repository identity is derived server-side and cannot be selected by a
// browser caller.
type IntegratedSetupRequest struct {
	Repository         string `json:"-"`
	RequestID          string `json:"request_id"`
	Coverage           string `json:"coverage"`
	Automation         string `json:"automation"`
	Provider           string `json:"provider"`
	VisualHiveRef      string `json:"visual_hive_ref"`
	ExpectedPlanSHA256 string `json:"expected_plan_sha256,omitempty"`
}

func (s *Server) handleIntegratedSetupPlan(w http.ResponseWriter, r *http.Request) {
	s.handleIntegratedSetup(w, r, false)
}

func (s *Server) handleIntegratedSetupApply(w http.ResponseWriter, r *http.Request) {
	s.handleIntegratedSetup(w, r, true)
}

func (s *Server) handleIntegratedSetup(w http.ResponseWriter, r *http.Request, apply bool) {
	if s.deps == nil || s.deps.Config == nil || s.deps.IntegratedSetupFunc == nil {
		jsonError(w, "integrated setup is unavailable", http.StatusServiceUnavailable)
		return
	}

	var request IntegratedSetupRequest
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16*1024))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		jsonError(w, "invalid integrated setup request: "+err.Error(), http.StatusBadRequest)
		return
	}
	if err := ensureJSONEOF(decoder); err != nil {
		jsonError(w, "invalid integrated setup request: "+err.Error(), http.StatusBadRequest)
		return
	}
	token, repository, ok := s.authorizeIntegratedOwner(w, r)
	if !ok {
		return
	}
	request.Repository = repository
	if err := validateIntegratedSetupRequest(request, apply); err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}

	action := "integrated_setup_plan"
	if apply {
		action = "integrated_setup_apply"
	}
	result, err := s.deps.IntegratedSetupFunc(r.Context(), request, token)
	if err != nil {
		s.writeIntegratedLifecycleError(w, r, strings.TrimPrefix(action, "integrated_"), request.Repository, token, err)
		return
	}
	s.auditFromRequest(r, action, "", auditDetail(
		"repository", request.Repository,
		"request_id", request.RequestID,
		"visual_hive_ref", request.VisualHiveRef,
		"expected_plan_sha256", request.ExpectedPlanSHA256,
	))
	jsonResponse(w, result)
}

func (s *Server) authorizeIntegratedOwner(w http.ResponseWriter, r *http.Request) (string, string, bool) {
	if s.deps == nil || s.deps.Config == nil {
		jsonError(w, "integrated lifecycle is unavailable", http.StatusServiceUnavailable)
		return "", "", false
	}
	if s.authToken == "" && !s.directRouteAuthzEnabled() {
		jsonError(w, "integrated lifecycle requires an authenticated dashboard", http.StatusUnauthorized)
		return "", "", false
	}
	role := strings.TrimSpace(r.Header.Get("X-Hive-Role"))
	if role != "owner" {
		jsonError(w, "only the hive owner may manage the integrated lifecycle", http.StatusForbidden)
		return "", "", false
	}
	tokenLoader := func() (string, error) {
		tokenData, err := os.ReadFile(userTokenPath)
		return strings.TrimSpace(string(tokenData)), err
	}
	if s.deps.IntegratedSetupTokenFunc != nil {
		tokenLoader = s.deps.IntegratedSetupTokenFunc
	}
	token, err := tokenLoader()
	if err != nil || strings.TrimSpace(token) == "" {
		jsonError(w, "owner GitHub authorization is required; complete the dashboard GitHub device-flow sign-in first", http.StatusConflict)
		return "", "", false
	}
	authorizer := func(value string) (string, error) {
		user, validateErr := github.ValidateToken(value, s.deps.Config.GitHub.ResolvedAPIURL())
		if validateErr != nil {
			return "", validateErr
		}
		if user == nil {
			return "", errors.New("GitHub user is unavailable")
		}
		return strings.TrimSpace(user.Login), nil
	}
	if s.deps.IntegratedSetupAuthorizerFunc != nil {
		authorizer = s.deps.IntegratedSetupAuthorizerFunc
	}
	username, err := authorizer(token)
	if err != nil || username == "" {
		jsonError(w, "saved owner GitHub authorization is invalid; sign in again through the dashboard", http.StatusConflict)
		return "", "", false
	}
	actor := strings.TrimSpace(r.Header.Get("X-Hive-User"))
	if actor != "" && !strings.EqualFold(actor, username) {
		jsonError(w, "the active dashboard owner does not match the saved GitHub setup authorizer", http.StatusForbidden)
		return "", "", false
	}
	repository := strings.TrimSpace(s.deps.Config.Project.PrimaryRepo)
	if repository == "" && len(s.deps.Config.Project.Repos) > 0 {
		repository = strings.TrimSpace(s.deps.Config.Project.Repos[0])
	}
	if !regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`).MatchString(repository) {
		jsonError(w, "the hive has no exact repository configured for the integrated lifecycle", http.StatusConflict)
		return "", "", false
	}
	return token, repository, true
}

func validateIntegratedSetupRequest(request IntegratedSetupRequest, apply bool) error {
	if !regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`).MatchString(request.Repository) {
		return errors.New("the hive has no exact repository configured for integrated setup")
	}
	if !integratedRequestIDPattern.MatchString(request.RequestID) {
		return errors.New("request_id must be 8-128 characters using letters, digits, dot, underscore, colon, or hyphen")
	}
	switch request.Coverage {
	case "essential", "standard", "comprehensive", "custom":
	default:
		return errors.New("coverage must be essential, standard, comprehensive, or custom")
	}
	switch request.Automation {
	case "advisory", "issues", "repair-pr", "auto-merge":
	default:
		return errors.New("automation must be advisory, issues, repair-pr, or auto-merge")
	}
	if request.Provider != "codex" {
		return errors.New("dashboard integrated setup currently requires provider codex")
	}
	if !integratedSetupCommitPattern.MatchString(request.VisualHiveRef) {
		return errors.New("visual_hive_ref must be an exact lowercase 40-character commit")
	}
	if apply {
		if !integratedSetupDigestPattern.MatchString(request.ExpectedPlanSHA256) {
			return errors.New("expected_plan_sha256 must bind the exact dashboard setup plan")
		}
	} else if request.ExpectedPlanSHA256 != "" {
		return errors.New("expected_plan_sha256 is accepted only by setup apply")
	}
	return nil
}

func ensureJSONEOF(decoder *json.Decoder) error {
	var extra any
	err := decoder.Decode(&extra)
	if errors.Is(err, io.EOF) {
		return nil
	}
	if err == nil {
		return errors.New("request contains more than one JSON value")
	}
	return err
}

func digestSetupError(err error) string {
	sum := sha256.Sum256([]byte(fmt.Sprint(err)))
	return hex.EncodeToString(sum[:])
}

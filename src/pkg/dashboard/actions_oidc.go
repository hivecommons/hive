package dashboard

import (
	"context"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/mention"
	"github.com/hivecommons/hive/pkg/outputschema"
)

const (
	githubActionsOIDCIssuer         = "https://token.actions.githubusercontent.com"
	actionsJWKSCacheTTL             = 10 * time.Minute
	actionsJWKSUnknownKidRefetchGap = time.Minute
	actionsJWKSMaxResponseBytes     = 1 << 20
	actionsDispatchMaxBody          = 32 << 10
)

type actionsOIDCClaims struct {
	Repository      string `json:"repository"`
	RepositoryOwner string `json:"repository_owner"`
	Actor           string `json:"actor"`
	Workflow        string `json:"workflow"`
	Ref             string `json:"ref"`
	RunID           string `json:"run_id"`
	RunAttempt      string `json:"run_attempt"`
	JTI             string `json:"jti"`
	jwt.RegisteredClaims
}

type actionsDispatchRequest struct {
	Command    string `json:"command"`
	Prompt     string `json:"prompt"`
	Issue      int    `json:"issue"`
	RunID      string `json:"run_id"`
	RunAttempt string `json:"run_attempt"`
}

type jwksDocument struct {
	Keys []jwkKey `json:"keys"`
}

type jwkKey struct {
	Kty string `json:"kty"`
	Use string `json:"use"`
	Kid string `json:"kid"`
	Alg string `json:"alg"`
	N   string `json:"n"`
	E   string `json:"e"`
}

func (s *Server) handleActionsDispatch(w http.ResponseWriter, r *http.Request) {
	if s.deps == nil || s.deps.Config == nil {
		jsonError(w, "actions dispatch unavailable", http.StatusServiceUnavailable)
		return
	}
	cfg := s.deps.Config.GitHub.Actions
	if !cfg.OIDC.Enabled {
		s.auditActionDispatch("refused", "transport", "oidc", "guard", "disabled")
		jsonError(w, "actions oidc dispatch is disabled", http.StatusForbidden)
		return
	}
	claims, err := s.verifyActionsOIDC(r.Context(), actionsBearerToken(r.Header.Get("Authorization")), cfg.OIDC)
	if err != nil {
		s.auditActionDispatch("refused", "transport", "oidc", "guard", "jwt", "detail", refusalDetail(err.Error()))
		jsonError(w, "invalid actions oidc token", http.StatusUnauthorized)
		return
	}
	var body actionsDispatchRequest
	r.Body = http.MaxBytesReader(w, r.Body, actionsDispatchMaxBody)
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		s.auditActionDispatch("refused", "transport", "oidc", "repo", claims.Repository, "actor", claims.Actor, "guard", "body")
		jsonError(w, "invalid dispatch body", http.StatusBadRequest)
		return
	}
	if bodyRun := strings.TrimSpace(body.RunID); bodyRun != "" && bodyRun != strings.TrimSpace(claims.RunID) {
		s.auditActionDispatch("refused", "transport", "oidc", "repo", claims.Repository, "actor", claims.Actor, "guard", "run")
		jsonError(w, "run_id does not match token", http.StatusBadRequest)
		return
	}
	if bodyAttempt := strings.TrimSpace(body.RunAttempt); bodyAttempt != "" && bodyAttempt != strings.TrimSpace(claims.RunAttempt) {
		s.auditActionDispatch("refused", "transport", "oidc", "repo", claims.Repository, "actor", claims.Actor, "guard", "run")
		jsonError(w, "run_attempt does not match token", http.StatusBadRequest)
		return
	}

	store := s.actionsMentionStore()
	now := s.actionsNow()
	runKey := actionsRunDedupeKey(claims)
	kickID := "action:" + runKey
	jtiKey := actionsJTIKey(claims)
	replay := store != nil && (store.Seen(runKey) || (jtiKey != "" && store.Seen(jtiKey)))
	event := mention.Event{
		Repo:      strings.TrimSpace(claims.Repository),
		Kind:      "issue",
		Number:    body.Issue,
		NodeID:    "actions-oidc:" + runKey,
		Author:    "github-actions[bot]",
		Body:      "@hive " + strings.TrimSpace(body.Command) + " " + strings.TrimSpace(body.Prompt),
		CreatedAt: now,
		UpdatedAt: now,
		Action:    mention.ActionMarker{Source: mention.SourceAction, RunID: strings.TrimSpace(claims.RunID), RunAttempt: strings.TrimSpace(claims.RunAttempt), Actor: claims.Actor, Workflow: claims.Workflow, Ref: claims.Ref, Transport: "oidc"},
	}

	kicked := false
	h := mention.NewHandler(mention.Options{
		Config:  s.deps.Config.GitHub.Mentions,
		Actions: cfg,
		Roles: func(login string) (string, bool) {
			return s.deps.Config.Dashboard.AuthorizedRole(login)
		},
		Repos:  s.actionsRepos,
		Agents: s.actionsAgents,
		GitHub: actionsGitHub{app: s.actionsAppLogin()},
		Store:  store,
		Kick: func(agent, message, source string) error {
			kicked = true
			return s.actionsKick(agent, message, source)
		},
		Audit: func(action, detail, agent string) {
			s.AuditLog("system", action, detail, agent)
		},
		Now: s.actionsNow,
	})
	if err := h.Handle(r.Context(), event); err != nil {
		s.auditActionDispatch("refused", "transport", "oidc", "repo", claims.Repository, "actor", claims.Actor, "guard", "handler", "detail", refusalDetail(err.Error()))
		jsonError(w, "actions dispatch failed", http.StatusBadGateway)
		return
	}
	if !kicked && !replay {
		s.auditActionDispatch("refused", "transport", "oidc", "repo", claims.Repository, "actor", claims.Actor, "workflow", claims.Workflow, "ref", claims.Ref, "run_id", claims.RunID, "run_attempt", claims.RunAttempt, "guard", "action")
		jsonError(w, "actions dispatch refused", http.StatusForbidden)
		return
	}
	if store != nil && jtiKey != "" {
		_ = store.Mark(jtiKey)
	}
	s.auditActionDispatch("accepted", "transport", "oidc", "repo", claims.Repository, "actor", claims.Actor, "workflow", claims.Workflow, "ref", claims.Ref, "run_id", claims.RunID, "run_attempt", claims.RunAttempt)
	receipt := actionsStageReceiptJSON(claims, body, kickID, now)
	s.auditActionDispatch("receipt", "transport", "oidc", "repo", claims.Repository, "actor", claims.Actor, "run_id", claims.RunID, "run_attempt", claims.RunAttempt)
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(map[string]any{"accepted": true, "kick_id": kickID, "receipt": receipt})
}

func actionsRunDedupeKey(claims actionsOIDCClaims) string {
	repo := strings.ToLower(strings.TrimSpace(claims.Repository))
	if repo == "" {
		repo = "unknown"
	}
	return repo + ":" + strings.TrimSpace(claims.RunID) + ":" + strings.TrimSpace(claims.RunAttempt)
}

func actionsJTIKey(claims actionsOIDCClaims) string {
	if strings.TrimSpace(claims.JTI) == "" {
		return ""
	}
	return "actions-oidc-jti:" + strings.TrimSpace(claims.JTI)
}

func actionsStageReceiptJSON(claims actionsOIDCClaims, body actionsDispatchRequest, kickID string, at time.Time) string {
	// StableDigest with no artifact inputs is SHA-256 over the empty stream; keep
	// the response validated without importing the receipt internals here.
	const emptyArtifactDigest = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	report := map[string]any{
		"lane":        "runs",
		"kind":        string(outputschema.KindStageReceipt),
		"findings":    []any{},
		"prs_opened":  []any{},
		"beads_filed": []any{},
		"summary":     "GitHub Actions OIDC dispatch accepted",
		"stage_receipt": map[string]any{
			"schema_version":     outputschema.StageReceiptSchemaVersion,
			"work_key":           strings.TrimSpace(claims.Repository) + "#" + strconv.Itoa(body.Issue),
			"assignment_id":      kickID,
			"generation":         1,
			"stage":              "dispatch",
			"contract_revision":  "runs-action/v1",
			"execution_key":      kickID + "|dispatch",
			"engine":             map[string]string{"name": "github-actions", "version": "oidc"},
			"remote_run_id":      strings.TrimSpace(claims.RunID),
			"remote_incarnation": strings.TrimSpace(claims.RunAttempt),
			"input_revision":     "0000000000000000000000000000000000000000",
			"output_digest":      emptyArtifactDigest,
			"result_class":       string(outputschema.ReceiptResultNoChange),
			"started_at":         at.UTC().Format(time.RFC3339Nano),
			"ended_at":           at.UTC().Format(time.RFC3339Nano),
			"provenance":         map[string]any{"query": "actions_oidc_dispatch@" + strings.TrimSpace(claims.RunID)},
			"artifacts":          []any{},
		},
	}
	raw, err := json.Marshal(report)
	if err != nil {
		return "{}"
	}
	if _, err := outputschema.Validate(raw); err != nil {
		return "{}"
	}
	return string(raw)
}

type actionsGitHub struct{ app string }

func (g actionsGitHub) AppBotLogin() string { return g.app }
func (g actionsGitHub) ListMentionComments(context.Context, string, time.Time) ([]mention.Event, error) {
	return nil, nil
}
func (g actionsGitHub) CreateMentionAck(context.Context, mention.Event, string) error { return nil }
func (g actionsGitHub) CountMentionReplies(context.Context, string, int) (int, error) {
	return 0, nil
}
func (g actionsGitHub) CreateIssueComment(context.Context, string, int, string) error { return nil }

func (s *Server) actionsKick(agent, message, source string) error {
	if s.deps != nil && s.deps.ActionDispatchKick != nil {
		return s.deps.ActionDispatchKick(agent, message, source)
	}
	if s.deps == nil || s.deps.AgentMgr == nil {
		return errors.New("agent manager unavailable")
	}
	if err := s.deps.AgentMgr.SendKickWithSource(agent, message, source); err != nil {
		return err
	}
	if s.deps.Governor != nil {
		s.deps.Governor.RecordKick(agent)
	}
	return nil
}

func (s *Server) actionsAgents() []mention.AgentInfo {
	if s.deps != nil && s.deps.ActionDispatchAgents != nil {
		return s.deps.ActionDispatchAgents()
	}
	if s.deps == nil || s.deps.Config == nil {
		return nil
	}
	out := make([]mention.AgentInfo, 0, len(s.deps.Config.Agents))
	for name, ac := range s.deps.Config.Agents {
		out = append(out, mention.AgentInfo{Name: name, Enabled: ac.Enabled, Converse: ac.Converse != nil && *ac.Converse, Mention: ac.HasEnabledChannel(config.ChannelTypeMention), GovernorKick: ac.UsesGovernorKick()})
	}
	return out
}

func (s *Server) actionsRepos() []string {
	if s.deps != nil && s.deps.GHClient != nil {
		return s.deps.GHClient.ActiveRepositories()
	}
	if s.deps == nil || s.deps.Config == nil {
		return nil
	}
	return append([]string(nil), s.deps.Config.Project.Repos...)
}

func (s *Server) actionsAppLogin() string {
	if s.deps != nil && s.deps.GHClient != nil && s.deps.GHClient.AppBotLogin() != "" {
		return s.deps.GHClient.AppBotLogin()
	}
	return "hive[bot]"
}

func (s *Server) actionsMentionStore() *mention.Store {
	if s.deps != nil && s.deps.MentionStore != nil {
		return s.deps.MentionStore
	}
	st, _ := mention.NewStore("")
	return st
}

func (s *Server) actionsNow() time.Time {
	if s.deps != nil && s.deps.ActionsClock != nil {
		return s.deps.ActionsClock()
	}
	return time.Now()
}

func (s *Server) auditActionDispatch(result string, kv ...string) {
	if s == nil {
		return
	}
	pairs := append([]string{"result", result}, kv...)
	s.AuditLog("system", "github_actions_dispatch", auditDetail(pairs...), "")
}

func refusalDetail(s string) string {
	s = strings.ReplaceAll(s, ",", ";")
	if len(s) > 160 {
		return s[:160]
	}
	return s
}

func actionsBearerToken(header string) string {
	parts := strings.Fields(strings.TrimSpace(header))
	if len(parts) == 2 && strings.EqualFold(parts[0], "Bearer") {
		return parts[1]
	}
	return ""
}

func (s *Server) verifyActionsOIDC(ctx context.Context, token string, cfg config.GitHubActionsOIDCConfig) (actionsOIDCClaims, error) {
	if strings.TrimSpace(token) == "" {
		return actionsOIDCClaims{}, errors.New("missing bearer token")
	}
	claims := actionsOIDCClaims{}
	_, err := jwt.ParseWithClaims(token, &claims, func(t *jwt.Token) (interface{}, error) {
		if t.Method != jwt.SigningMethodRS256 {
			return nil, fmt.Errorf("unexpected signing method %s", t.Header["alg"])
		}
		kid, _ := t.Header["kid"].(string)
		if kid == "" {
			return nil, errors.New("missing kid")
		}
		keys, err := s.actionsJWKS(ctx, cfg.JWKSURLEffective(), false)
		if err != nil {
			return nil, err
		}
		key, ok := keys[kid]
		if !ok {
			keys, err = s.actionsJWKS(ctx, cfg.JWKSURLEffective(), true)
			if err != nil {
				return nil, err
			}
			key, ok = keys[kid]
			if !ok {
				return nil, errors.New("unknown kid")
			}
		}
		return key, nil
	}, jwt.WithIssuer(githubActionsOIDCIssuer), jwt.WithAudience(strings.TrimSpace(cfg.Audience)), jwt.WithLeeway(cfg.MaxSkewEffective()), jwt.WithExpirationRequired(), jwt.WithIssuedAt(), jwt.WithTimeFunc(s.actionsNow))
	if err != nil {
		return actionsOIDCClaims{}, err
	}
	if strings.TrimSpace(claims.Repository) == "" || strings.TrimSpace(claims.Actor) == "" || strings.TrimSpace(claims.RunID) == "" || strings.TrimSpace(claims.RunAttempt) == "" || strings.TrimSpace(claims.JTI) == "" {
		return actionsOIDCClaims{}, errors.New("missing required actions claims")
	}
	return claims, nil
}

func (s *Server) actionsJWKS(ctx context.Context, url string, force bool) (map[string]interface{}, error) {
	now := s.actionsNow()
	s.actionsJWKSMu.Lock()
	if force && !s.actionsJWKSLastRefetch.IsZero() && now.Sub(s.actionsJWKSLastRefetch) < actionsJWKSUnknownKidRefetchGap {
		keys := s.actionsJWKSKeys
		s.actionsJWKSMu.Unlock()
		if keys == nil {
			return nil, errors.New("jwks refetch rate limited")
		}
		return keys, nil
	}
	if !force && s.actionsJWKSKeys != nil && now.Before(s.actionsJWKSUntil) {
		keys := s.actionsJWKSKeys
		s.actionsJWKSMu.Unlock()
		return keys, nil
	}
	if force {
		s.actionsJWKSLastRefetch = now
	}
	s.actionsJWKSMu.Unlock()
	fetch := fetchJWKSHTTP
	if s.deps != nil && s.deps.ActionsJWKSFetcher != nil {
		fetch = s.deps.ActionsJWKSFetcher
	}
	b, err := fetch(ctx, url)
	if err != nil {
		return nil, err
	}
	keys, err := parseJWKS(b)
	if err != nil {
		return nil, err
	}
	s.actionsJWKSMu.Lock()
	s.actionsJWKSKeys = keys
	s.actionsJWKSUntil = now.Add(actionsJWKSCacheTTL)
	s.actionsJWKSMu.Unlock()
	return keys, nil
}

func fetchJWKSHTTP(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("jwks fetch status %d", resp.StatusCode)
	}
	limited := io.LimitReader(resp.Body, actionsJWKSMaxResponseBytes+1)
	b, err := io.ReadAll(limited)
	if err != nil {
		return nil, err
	}
	if len(b) > actionsJWKSMaxResponseBytes {
		return nil, errors.New("jwks response too large")
	}
	return b, nil
}

func parseJWKS(b []byte) (map[string]interface{}, error) {
	var doc jwksDocument
	if err := json.Unmarshal(b, &doc); err != nil {
		return nil, err
	}
	out := map[string]interface{}{}
	for _, k := range doc.Keys {
		if k.Kty != "RSA" || k.Kid == "" || (k.Use != "" && k.Use != "sig") || (k.Alg != "" && k.Alg != "RS256") {
			continue
		}
		pub, err := jwkRSAKey(k)
		if err != nil {
			return nil, err
		}
		out[k.Kid] = pub
	}
	return out, nil
}

func jwkRSAKey(k jwkKey) (*rsa.PublicKey, error) {
	nBytes, err := base64.RawURLEncoding.DecodeString(k.N)
	if err != nil {
		return nil, err
	}
	eBytes, err := base64.RawURLEncoding.DecodeString(k.E)
	if err != nil {
		return nil, err
	}
	e := 0
	for _, b := range eBytes {
		e = e<<8 + int(b)
	}
	if e == 0 {
		return nil, errors.New("invalid rsa exponent")
	}
	return &rsa.PublicKey{N: new(big.Int).SetBytes(nBytes), E: e}, nil
}

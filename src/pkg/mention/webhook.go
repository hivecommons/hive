package mention

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	githubSignatureHeader = "X-Hub-Signature-256"
	githubEventHeader     = "X-GitHub-Event"
	webhookMaxBody        = 256 << 10
	webhookDefaultMinGap  = 30 * time.Second
)

type WebhookPoller interface {
	PollRepo(context.Context, string)
}

type WebhookReceiver struct {
	secret      func() string
	repos       func() []string
	poller      WebhookPoller
	minInterval time.Duration
	logger      *slog.Logger
	now         func() time.Time

	mu       sync.Mutex
	lastPoll map[string]time.Time
}

func NewWebhookReceiver(secret func() string, poller WebhookPoller, minInterval time.Duration, logger *slog.Logger) *WebhookReceiver {
	if logger == nil {
		logger = slog.Default()
	}
	if minInterval <= 0 {
		minInterval = webhookDefaultMinGap
	}
	if secret == nil || strings.TrimSpace(secret()) == "" {
		logger.Warn("mention: webhook accelerator secret is not configured; deliveries will be ignored")
	}
	return &WebhookReceiver{
		secret:      secret,
		poller:      poller,
		minInterval: minInterval,
		logger:      logger,
		now:         time.Now,
		lastPoll:    map[string]time.Time{},
	}
}

func (w *WebhookReceiver) SetReposFunc(f func() []string) {
	w.repos = f
}

func (w *WebhookReceiver) SetClock(f func() time.Time) {
	w.now = f
}

func (w *WebhookReceiver) ServeHTTP(rw http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(rw, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, webhookMaxBody))
	if err != nil {
		rw.WriteHeader(http.StatusNoContent)
		return
	}
	if !w.verify(body, r.Header.Get(githubSignatureHeader)) {
		rw.WriteHeader(http.StatusNoContent)
		return
	}
	repo, ok := webhookRepo(r.Header.Get(githubEventHeader), body)
	if ok {
		repo, ok = w.configuredRepo(repo)
	}
	if ok && w.admit(repo) && w.poller != nil {
		go w.poller.PollRepo(context.Background(), repo)
	}
	rw.WriteHeader(http.StatusNoContent)
}

func (w *WebhookReceiver) configuredRepo(repo string) (string, bool) {
	if w == nil || w.repos == nil {
		return repo, strings.TrimSpace(repo) != ""
	}
	want := strings.TrimSpace(repo)
	for _, configured := range w.repos() {
		if strings.EqualFold(strings.TrimSpace(configured), want) {
			return strings.TrimSpace(configured), strings.TrimSpace(configured) != ""
		}
	}
	return "", false
}

func (w *WebhookReceiver) verify(body []byte, signature string) bool {
	if w == nil || w.secret == nil {
		return false
	}
	secret := strings.TrimSpace(w.secret())
	if secret == "" {
		return false
	}
	sig := strings.TrimSpace(signature)
	if !strings.HasPrefix(sig, "sha256=") {
		return false
	}
	got, err := hex.DecodeString(strings.TrimPrefix(sig, "sha256="))
	if err != nil || len(got) == 0 {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return hmac.Equal(got, mac.Sum(nil))
}

func (w *WebhookReceiver) admit(repo string) bool {
	now := w.now()
	w.mu.Lock()
	defer w.mu.Unlock()
	if last := w.lastPoll[repo]; !last.IsZero() && now.Sub(last) < w.minInterval {
		w.logger.Debug("mention: webhook accelerator coalesced", "repo", repo)
		return false
	}
	w.lastPoll[repo] = now
	return true
}

func webhookRepo(event string, body []byte) (string, bool) {
	switch event {
	case "issue_comment", "pull_request_review_comment", "issues":
	default:
		return "", false
	}
	var payload struct {
		Action     string `json:"action"`
		Repository struct {
			FullName string `json:"full_name"`
		} `json:"repository"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return "", false
	}
	if (event == "issues" && payload.Action != "opened") || (event != "issues" && payload.Action != "created") {
		return "", false
	}
	repo := strings.TrimSpace(payload.Repository.FullName)
	return repo, repo != ""
}

package notify

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

func TestSendDiscordWebhook_SuppressesMentions(t *testing.T) {
	var body string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		body = string(b)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	n := New(config.NotificationsConfig{
		Discord: &config.DiscordConfig{Webhook: srv.URL},
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	n.sendDiscordWebhook("RFC: @everyone", "<@&123>")

	if !strings.Contains(body, `"allowed_mentions":{"parse":[]}`) {
		t.Errorf("allowed_mentions.parse must be an empty array, body: %s", body)
	}
	if !strings.Contains(body, "**RFC: @everyone**") {
		t.Errorf("message wording changed, body: %s", body)
	}
}

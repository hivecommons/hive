package dashboard

import (
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
)

func TestAgentConfigChannelsRejectsRemovedRuntimesWithoutMutation(t *testing.T) {
	s := covCServer(t)
	before := s.deps.Config.Agents["scanner"]
	before.Channels = []config.ChannelConfig{{Type: "kick"}}
	s.deps.Config.Agents["scanner"] = before
	persisted := make(chan struct{}, 1)
	s.deps.PersistFunc = func() { persisted <- struct{}{} }

	for _, channelType := range []string{"webhook", "discord", "schedule", "bead", "invalid"} {
		rec := doPut(s, "/api/config/agent/scanner/channels", map[string]any{
			"channels": []map[string]any{{"type": "kick"}, {"type": channelType, "events": []string{"issues.opened"}}},
		})
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%s: status = %d, body = %s", channelType, rec.Code, rec.Body.String())
		}
		body := decodeJSON(t, rec)
		msg, _ := body["error"].(string)
		if !strings.Contains(msg, "no trigger runtime") || !strings.Contains(msg, `"kick" is supported`) {
			t.Fatalf("%s: missing actionable validation error: %v", channelType, body)
		}
		if !reflect.DeepEqual(s.deps.Config.Agents["scanner"], before) {
			t.Fatalf("%s: rejected save changed the existing agent config", channelType)
		}
	}
	select {
	case <-persisted:
		t.Fatal("invalid channels must not be persisted")
	default:
	}

	rec := doPut(s, "/api/config/agent/scanner/channels", map[string]any{
		"channels": []map[string]any{{"type": "kick"}, {"type": "kick", "enabled": false}},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("supported save: %d %s", rec.Code, rec.Body.String())
	}
	select {
	case <-persisted:
	case <-time.After(time.Second):
		t.Fatal("supported save did not request persistence")
	}
	got := s.deps.Config.Agents["scanner"]
	if len(got.Channels) != 2 || got.Channels[1].IsEnabled() {
		t.Fatalf("supported channels not saved: %+v", got.Channels)
	}
	got.Channels = before.Channels
	if !reflect.DeepEqual(got, before) {
		t.Fatal("saving channels modified unrelated agent settings")
	}
}

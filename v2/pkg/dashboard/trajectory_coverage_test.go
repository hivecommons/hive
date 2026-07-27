package dashboard

import (
	"net/http"
	"testing"

	"github.com/kubestellar/hive/v2/pkg/config"
	"github.com/kubestellar/hive/v2/pkg/notify"
)

func TestCov_HandleGovernorTrajectory(t *testing.T) {
	t.Run("bad_body", func(t *testing.T) {
		s, _ := covServer(t)
		rec := doPut(s, "/api/config/governor/trajectory", "not-an-object")
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
		}
	})

	t.Run("all_fields", func(t *testing.T) {
		s, deps := covServer(t)
		enabled := true
		iv := 120
		tl := 40
		body := map[string]interface{}{
			"enabled":         enabled,
			"intervalS":       iv,
			"model":           "  cheap-model  ",
			"endpoint":        "  https://reviewer.local  ",
			"transcriptLines": tl,
			"onDivergence":    "pause",
		}
		rec := doPut(s, "/api/config/governor/trajectory", body)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
		}
		tr := deps.Config.Governor.Trajectory
		if tr.Enabled == nil || !*tr.Enabled {
			t.Error("enabled not set")
		}
		if tr.IntervalS != 120 {
			t.Errorf("intervalS = %d", tr.IntervalS)
		}
		if tr.Model != "cheap-model" {
			t.Errorf("model = %q (want trimmed)", tr.Model)
		}
		if tr.Endpoint != "https://reviewer.local" {
			t.Errorf("endpoint = %q", tr.Endpoint)
		}
		if tr.TranscriptLines != 40 {
			t.Errorf("transcriptLines = %d", tr.TranscriptLines)
		}
		if tr.OnDivergence != "pause" {
			t.Errorf("onDivergence = %q", tr.OnDivergence)
		}
	})

	t.Run("disable_and_alert", func(t *testing.T) {
		s, deps := covServer(t)
		body := map[string]interface{}{
			"enabled":      false,
			"onDivergence": "alert",
		}
		rec := doPut(s, "/api/config/governor/trajectory", body)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d", rec.Code)
		}
		tr := deps.Config.Governor.Trajectory
		if tr.Enabled == nil || *tr.Enabled {
			t.Error("expected enabled=false")
		}
		if tr.OnDivergence != "alert" {
			t.Errorf("onDivergence = %q", tr.OnDivergence)
		}
	})

	t.Run("invalid_ondivergence_ignored", func(t *testing.T) {
		s, deps := covServer(t)
		deps.Config.Governor.Trajectory.OnDivergence = "pause"
		body := map[string]interface{}{"onDivergence": "garbage"}
		rec := doPut(s, "/api/config/governor/trajectory", body)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d", rec.Code)
		}
		// Invalid value leaves the prior value untouched.
		if deps.Config.Governor.Trajectory.OnDivergence != "pause" {
			t.Errorf("onDivergence changed to %q on invalid input", deps.Config.Governor.Trajectory.OnDivergence)
		}
	})

	t.Run("negative_intervals_ignored", func(t *testing.T) {
		s, deps := covServer(t)
		deps.Config.Governor.Trajectory.IntervalS = 300
		deps.Config.Governor.Trajectory.TranscriptLines = 50
		body := map[string]interface{}{"intervalS": -1, "transcriptLines": -5}
		rec := doPut(s, "/api/config/governor/trajectory", body)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d", rec.Code)
		}
		if deps.Config.Governor.Trajectory.IntervalS != 300 {
			t.Errorf("negative intervalS was applied")
		}
		if deps.Config.Governor.Trajectory.TranscriptLines != 50 {
			t.Errorf("negative transcriptLines was applied")
		}
	})
}

func TestCov_ReconcileTrajectoryAlert(t *testing.T) {
	s, deps := covServer(t)
	g := &deps.Config.Governor

	// Enabled + no reviewer endpoint/model → alert raised.
	enabled := true
	g.Trajectory.Enabled = &enabled
	g.Trajectory.Endpoint = ""
	g.Trajectory.Model = ""
	g.LiteLLM.Endpoint = ""
	g.LiteLLM.DefaultModel = ""
	s.ReconcileTrajectoryAlert(g)
	if !covHasAlert(s, TrajectoryNotConfiguredAlertID) {
		t.Error("expected trajectory-not-configured alert to be raised")
	}

	// Disabled → alert cleared.
	disabled := false
	g.Trajectory.Enabled = &disabled
	s.ReconcileTrajectoryAlert(g)
	if covHasAlert(s, TrajectoryNotConfiguredAlertID) {
		t.Error("expected alert cleared when disabled")
	}

	// Enabled + reviewer ready → alert cleared.
	g.Trajectory.Enabled = &enabled
	g.Trajectory.Endpoint = "https://reviewer.local"
	g.Trajectory.Model = "cheap-model"
	s.ReconcileTrajectoryAlert(g)
	if covHasAlert(s, TrajectoryNotConfiguredAlertID) {
		t.Error("expected alert cleared when reviewer ready")
	}
}

func TestCov_TrajectorySectionResponse(t *testing.T) {
	g := &config.GovernorConfig{}
	enabled := true
	g.Trajectory.Enabled = &enabled

	// endpoint from trajectory block
	g.Trajectory.Endpoint = "https://tj.local"
	g.Trajectory.Model = "m"
	resp := trajectorySectionResponse(g)
	if resp["endpointSource"] != "trajectory" {
		t.Errorf("endpointSource = %v, want trajectory", resp["endpointSource"])
	}
	if resp["reviewerReady"] != true {
		t.Errorf("reviewerReady = %v, want true", resp["reviewerReady"])
	}
	for _, k := range []string{"enabled", "intervalS", "model", "effectiveModel", "transcriptLines", "onDivergence", "exemptAgents", "endpoint", "effectiveEndpoint", "endpointSource", "hasKey", "reviewerReady"} {
		if _, ok := resp[k]; !ok {
			t.Errorf("missing key %q in trajectory section response", k)
		}
	}

	// endpoint inherited from litellm block
	g2 := &config.GovernorConfig{}
	g2.Trajectory.Endpoint = ""
	g2.LiteLLM.Endpoint = "https://litellm.local"
	g2.LiteLLM.DefaultModel = "lm"
	resp2 := trajectorySectionResponse(g2)
	if resp2["endpointSource"] != "litellm" {
		t.Errorf("endpointSource = %v, want litellm", resp2["endpointSource"])
	}

	// no endpoint anywhere
	g3 := &config.GovernorConfig{}
	resp3 := trajectorySectionResponse(g3)
	if resp3["endpointSource"] != "none" {
		t.Errorf("endpointSource = %v, want none", resp3["endpointSource"])
	}
}

func TestCov_TrajectorySink(t *testing.T) {
	s, _ := covServer(t)

	// nil notifier: Notify is a no-op branch.
	sink := NewTrajectorySink(s, nil)
	sink.Audit("scanner", "review", "detail")
	sink.Alert("some-id", "warning", "message")
	sink.Notify("title", "message")

	// non-nil notifier: exercises the notifier != nil branch of Notify.
	n := notify.New(config.NotificationsConfig{}, covLogger())
	sink2 := NewTrajectorySink(s, n)
	sink2.Notify("title2", "message2")
}

// covHasAlert reports whether the server currently holds a system alert with id.
func covHasAlert(s *Server, id string) bool {
	s.systemAlertsMu.Lock()
	defer s.systemAlertsMu.Unlock()
	for _, a := range s.systemAlerts {
		if a.ID == id {
			return true
		}
	}
	return false
}

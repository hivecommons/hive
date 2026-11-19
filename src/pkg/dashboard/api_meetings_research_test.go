package dashboard

import (
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

func TestMeetingsResearchView(t *testing.T) {
	on := config.MeetingsConfig{Enabled: true}
	if v := meetingsResearchViewOf(config.MeetingsConfig{}, nil); v.Wired || v.Warning != "" || v.PoolSize != config.DefaultMeetingsResearchPoolSize {
		t.Fatalf("meetings off: %+v", v)
	}
	if v := meetingsResearchViewOf(on, nil); v.Wired || !strings.HasPrefix(v.Warning, "No research agent available: this hive has no research pool") {
		t.Fatalf("no pool: %+v", v)
	}
	none := func() MeetingsResearchStatus {
		return MeetingsResearchStatus{Agents: []string{"research-1", "research-2"}, BaseAgent: "scanner"}
	}
	v := meetingsResearchViewOf(on, none)
	if !v.Wired || !strings.Contains(v.Warning, "research-1, research-2 are not registered with the runtime or are paused") {
		t.Fatalf("none available: %+v", v)
	}
	ok := func() MeetingsResearchStatus {
		return MeetingsResearchStatus{Agents: []string{"research-1"}, Available: 1, BaseAgent: "scanner", Backend: "claude"}
	}
	if v := meetingsResearchViewOf(on, ok); v.Warning != "" || v.Available != 1 || v.BaseAgent != "scanner" {
		t.Fatalf("available: %+v", v)
	}

	s := &Server{}
	s.SetMeetingsResearchStatus(ok)
	if v := s.meetingsResearchView(on); !v.Wired || v.Backend != "claude" {
		t.Fatalf("server view: %+v", v)
	}
	s.SetMeetingsResearchStatus(nil)
	if v := s.meetingsResearchView(on); v.Wired {
		t.Fatal("nil removes the report")
	}
}

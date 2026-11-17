package main

import (
	"log/slog"
	"testing"

	"github.com/hivecommons/hive/pkg/beads"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/knowledge"
	"github.com/hivecommons/hive/pkg/meetings"
)

func TestMeetingPersonaFactory(t *testing.T) {
	store, err := meetings.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	on := true
	cfg := &config.Config{}
	cfg.Knowledge.CodeMaps.Enabled = &on
	persona := cfg.Meetings.Persona.Resolve()
	llm := config.ResolvedMeetingsLLM{Backend: config.MeetingsLLMModeOpenAI, Endpoint: "http://127.0.0.1:1", Model: "m", MaxInFlight: 1}
	beadStores := func() map[string]*beads.Store { return nil }
	fn := meetingPersonaFactory(cfg, persona, llm, store, &knowledge.KnowledgeAPI{}, beadStores, t.TempDir(), 100, true, slog.New(slog.DiscardHandler), nil)
	if fn == nil {
		t.Fatal("the persona is on by default")
	}
	e := fn(meetings.Meeting{ID: "mtg-1"})
	if e == nil || e.Name() != "Betty" {
		t.Fatal("want Betty")
	}
	e.Close()
	// With only an Anthropic key and a web search service she answers
	// through the Messages API and searches the web.
	web := cfg.Meetings.Persona
	web.SearxngURL = "http://searxng.meetings.svc.cluster.local:8080"
	anth := config.ResolvedMeetingsLLM{Backend: config.MeetingsLLMModeAnthropic, AnthropicKey: "ak", Model: config.DefaultMeetingsPersonaAnthropicModel, MaxInFlight: 1}
	if e := meetingPersonaFactory(cfg, web.Resolve(), anth, store, nil, nil, "", 0, false, nil, nil)(meetings.Meeting{ID: "m3"}); e == nil {
		t.Fatal("want an engine on the Anthropic path")
	} else {
		e.Close()
	}
	// Without a model Betty still runs (intros only).
	if e := meetingPersonaFactory(cfg, persona, config.ResolvedMeetingsLLM{Backend: config.MeetingsLLMModeNone}, store, nil, nil, "", 0, false, nil, nil)(meetings.Meeting{ID: "m2"}); e == nil {
		t.Fatal("want an engine without a model")
	}
	off := false
	cfg.Meetings.Persona.Enabled = &off
	if meetingPersonaFactory(cfg, cfg.Meetings.Persona.Resolve(), llm, store, nil, nil, "", 0, false, nil, nil) != nil {
		t.Fatal("persona off means no factory")
	}
	r := wireMeetingsLive(cfg, mustManager(t, store), store, nil, nil, nil, nil, nil)
	if r == nil {
		t.Fatal("live loop without the persona")
	}
	r.Close()
}

func mustManager(t *testing.T, store *meetings.Store) *meetings.Manager {
	t.Helper()
	mgr, err := meetings.NewManager(meetings.Options{Store: store, Quorum: meetings.NewClient("http://127.0.0.1:1", "t", nil)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(mgr.Close)
	return mgr
}

package dashboard

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gorilla/websocket"
)

func TestCampaignJamCallerAttributionAndSingleVote(t *testing.T) {
	s := jamTestServer(t)
	path := "/api/campaigns/spec-identity/jam"
	want := CampaignJamActor{Type: "human", Name: "mallory"}
	post := func(endpoint, role string, body map[string]any) CampaignJamState {
		t.Helper()
		body["agent"] = "spektacular"
		body["model"] = "forged-model"
		rec := jamPostAs(t, s, path+endpoint, role, "mallory", body, false)
		if rec.Code != http.StatusOK {
			t.Fatalf("post %s: %d %s", endpoint, rec.Code, rec.Body.String())
		}
		return decodeJam(t, rec)
	}
	assertActor := func(actor CampaignJamActor) {
		t.Helper()
		if actor != want {
			t.Fatalf("caller attribution = %+v, want %+v", actor, want)
		}
	}
	assertRevision := func(rev CampaignRevision) {
		t.Helper()
		assertActor(rev.Author)
		if rev.Agent != "" || rev.Model != "" {
			t.Fatalf("caller revision has agent/model metadata: %+v", rev)
		}
	}

	jam := post("", "read-write", map[string]any{"spec_content": "## Goals\nInitial"})
	assertRevision(jam.Revisions[0])
	jam = post("/threads", "read-write", map[string]any{"section": "Goals", "body": "Comment"})
	threadID := jam.Threads[0].ID
	jam = post("/threads", "read-write", map[string]any{"thread_id": threadID, "body": "Follow-up"})
	assertActor(jam.Threads[0].CreatedBy)
	for _, comment := range jam.Threads[0].Comments {
		assertActor(comment.Author)
	}
	jam = post("/suggestions", "read-write", map[string]any{"section": "Goals", "proposed_text": "Changed"})
	assertActor(jam.Suggestions[0].Author)
	jam = post("/suggestions", "merger", map[string]any{"action": "accept", "suggestion_id": jam.Suggestions[0].ID})
	assertRevision(jam.Revisions[len(jam.Revisions)-1])
	if jam.Suggestions[0].ResolvedBy == nil {
		t.Fatal("accepted suggestion missing resolver")
	}
	assertActor(*jam.Suggestions[0].ResolvedBy)
	jam = post("/polls", "read-write", map[string]any{"section": "Goals", "question": "Proceed?", "options": []string{"yes", "no"}})
	poll := jam.Polls[0]
	assertActor(poll.CreatedBy)
	for i := 0; i < 5; i++ {
		rec := jamPostAs(t, s, path+"/polls", "read-write", "mallory", map[string]any{
			"action": "vote", "poll_id": poll.ID, "option_id": poll.Options[i%2].ID,
			"agent": fmt.Sprintf("bot%d", i), "model": fmt.Sprintf("model%d", i),
		}, false)
		if rec.Code != http.StatusOK {
			t.Fatalf("vote %d: %d %s", i, rec.Code, rec.Body.String())
		}
		jam = decodeJam(t, rec)
		poll = jam.Polls[0]
		if len(poll.Votes) != 1 || poll.Options[i%2].Count != 1 || poll.Options[(i+1)%2].Count != 0 {
			t.Fatalf("caller accumulated votes instead of replacing vote: %+v", poll)
		}
		assertActor(poll.Votes[0].Voter)
	}
	// A different authenticated user still gets an independent vote.
	rec := jamPostAs(t, s, path+"/polls", "read-write", "alice", map[string]any{
		"action": "vote", "poll_id": poll.ID, "option_id": poll.Options[0].ID,
		"agent": "spektacular", "model": "forged-model",
	}, false)
	if rec.Code != http.StatusOK {
		t.Fatalf("alice vote: %d %s", rec.Code, rec.Body.String())
	}
	jam = decodeJam(t, rec)
	if len(jam.Polls[0].Votes) != 2 || jam.Polls[0].Options[0].Count != 2 {
		t.Fatalf("independent vote lost: %+v", jam.Polls[0])
	}
	jam = post("/polls", "merger", map[string]any{"action": "decide", "poll_id": poll.ID, "outcome": "yes", "rationale": "Proceed"})
	assertActor(jam.Polls[0].Decision.DecidedBy)
	assertRevision(jam.Revisions[len(jam.Revisions)-1])
	// Attribution is persisted, not merely sanitized in the response.
	stored, err := s.loadCampaignJam("spec-identity")
	if err != nil {
		t.Fatal(err)
	}
	for _, rev := range stored.Revisions {
		assertRevision(rev)
	}
}

func TestCampaignJamLiveIgnoresCallerAgentModel(t *testing.T) {
	for _, role := range []string{"read", "read-write"} {
		t.Run(role, func(t *testing.T) {
			s := jamTestServer(t)
			srv := httptest.NewServer(s.mux)
			t.Cleanup(srv.Close)
			header := http.Header{}
			header.Set("X-Hive-User", "mallory")
			header.Set("X-Hive-Role", role)
			url := "ws" + strings.TrimPrefix(srv.URL, "http") + "/api/campaigns/spec-live-identity-" + role + "/jam/ws?agent=architect&model=forged-model"
			conn, _, err := websocket.DefaultDialer.Dial(url, header)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = conn.Close() })
			want := CampaignJamActor{Type: "human", Name: "mallory"}
			snapshot := readJamLiveUntil(t, conn, jamLiveSnapshot, nil)
			if len(snapshot.Presence) != 1 || snapshot.Presence[0].Actor != want {
				t.Fatalf("forged presence: %+v", snapshot.Presence)
			}
			forged := CampaignJamActor{Type: "agent", Name: "architect", Agent: "architect", Model: "forged-model"}
			if err := conn.WriteJSON(jamLiveMessage{Type: jamLiveEdit, Content: "## Goals\nCaller text", Actor: &forged}); err != nil {
				t.Fatal(err)
			}
			if role == "read" {
				msg := readJamLiveUntil(t, conn, jamLiveError, nil)
				if msg.Error != "read-write access required" {
					t.Fatalf("read-only edit error: %+v", msg)
				}
				return
			}
			applied := readJamLiveUntil(t, conn, jamLiveEditApplied, nil)
			rev := applied.Jam.Revisions[0]
			if applied.Actor == nil || *applied.Actor != want || rev.Author != want || rev.Agent != "" || rev.Model != "" {
				t.Fatalf("forged live edit attribution: %+v", applied)
			}
		})
	}
}

package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	hivegithub "github.com/kubestellar/hive/pkg/github"
)

func TestDashboardOperatorHandoffIsBoundExpiringAndOneUse(t *testing.T) {
	originalExec, originalRead := dashboardOperatorHandoffExec, dashboardOperatorHandoffRead
	originalNow, originalRoot := dashboardOperatorHandoffNow, dashboardOperatorHandoffRoot
	t.Cleanup(func() {
		dashboardOperatorHandoffExec, dashboardOperatorHandoffRead = originalExec, originalRead
		dashboardOperatorHandoffNow, dashboardOperatorHandoffRoot = originalNow, originalRoot
	})

	now := time.Date(2026, time.August, 16, 12, 0, 0, 0, time.UTC)
	dashboardOperatorHandoffNow = func() time.Time { return now }
	dashboardOperatorHandoffRoot = func() string { return "/fixed/operator-handoffs" }
	stored := map[string][]byte{}
	dashboardOperatorHandoffExec = func(_ context.Context, operation, path string, input []byte) error {
		switch operation {
		case writeOperatorHandoffCommand:
			if _, exists := stored[path]; exists {
				return errors.New("exclusive create rejected duplicate")
			}
			stored[path] = append([]byte(nil), input...)
		case removeOperatorHandoffCommand:
			delete(stored, path)
		default:
			return errors.New("unexpected operation")
		}
		return nil
	}
	dashboardOperatorHandoffRead = func(_ context.Context, operation, path string) ([]byte, error) {
		if operation != consumeOperatorHandoffCommand {
			return nil, errors.New("unexpected operation")
		}
		data, exists := stored[path]
		if !exists {
			return nil, errors.New("handoff already consumed")
		}
		delete(stored, path)
		return append([]byte(nil), data...), nil
	}

	actor := hivegithub.AuthenticatedUserIdentity{ID: 101, Login: "DavidDiaz0317", Type: "User"}
	runtime := testLiveAppRuntimeSnapshot()
	runtime.Repository = "Owner/Repository"
	runtime.App.Repository = "owner/repository"
	requestID := "setup-request-1234"
	plan := strings.Repeat("a", 64)
	path, err := createDashboardOperatorHandoff(context.Background(), "owner/repository", requestID, plan, actor, runtime)
	if err != nil {
		t.Fatal(err)
	}
	if !pathWithinDashboardOperatorRoot(dashboardOperatorHandoffRoot(), path) || len(stored[path]) == 0 {
		t.Fatalf("handoff was not created under the fixed root: path=%q", path)
	}
	handoff, err := consumeDashboardOperatorHandoff(context.Background(), path, "OWNER/REPOSITORY", requestID, plan)
	if err != nil {
		t.Fatal(err)
	}
	if handoff.Actor.ID != actor.ID || handoff.Writer.ID != runtime.Writer.ID || handoff.App.AppID != runtime.App.AppID || handoff.BindingSHA256 == "" {
		t.Fatalf("consumed handoff lost its exact identity binding: %+v", handoff)
	}
	if _, err := consumeDashboardOperatorHandoff(context.Background(), path, "owner/repository", requestID, plan); err == nil || !strings.Contains(err.Error(), "already consumed") {
		t.Fatalf("one-use handoff replay was accepted: %v", err)
	}

	path, err = createDashboardOperatorHandoff(context.Background(), "owner/repository", requestID, plan, actor, runtime)
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(dashboardOperatorHandoffTTL + time.Second)
	if _, err := consumeDashboardOperatorHandoff(context.Background(), path, "owner/repository", requestID, plan); err == nil || !strings.Contains(err.Error(), "expired") {
		t.Fatalf("expired handoff was accepted: %v", err)
	}
}

func TestDashboardOperatorHandoffRejectsBindingAndPathDrift(t *testing.T) {
	handoff := dashboardOperatorHandoff{
		SchemaVersion: dashboardOperatorHandoffSchema, Repository: "owner/repository", RequestID: "setup-request-1234",
		ExpectedPlanSHA256: strings.Repeat("a", 64),
		Actor:              hivegithub.AuthenticatedUserIdentity{ID: 101, Login: "owner", Type: "User"},
		Writer:             hivegithub.AuthenticatedUserIdentity{ID: 202, Login: "hive[bot]", Type: "Bot"},
		IssuedAt:           time.Now().UTC(), ExpiresAt: time.Now().UTC().Add(time.Minute), Nonce: strings.Repeat("b", 48),
	}
	digest, err := dashboardOperatorHandoffDigest(handoff)
	if err != nil {
		t.Fatal(err)
	}
	handoff.BindingSHA256 = digest
	handoff.Repository = "owner/other"
	if current, _ := dashboardOperatorHandoffDigest(handoff); current == handoff.BindingSHA256 {
		t.Fatal("repository mutation did not invalidate the handoff digest")
	}
	for _, path := range []string{"/fixed/operator-handoffs", "/fixed/other.json", "/fixed/operator-handoffs/../escape.json"} {
		if pathWithinDashboardOperatorRoot("/fixed/operator-handoffs", path) {
			t.Fatalf("unsafe handoff path %q was accepted", path)
		}
	}
}

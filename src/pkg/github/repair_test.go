package github

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestUpsertRepairPullRequestCreatesThenUpdates(t *testing.T) {
	marker := "<!-- hive-repair: owner/repo:fingerprint -->"
	head := strings.Repeat("a", 40)
	listCalls, createCalls, editCalls := 0, 0, 0
	title, body := "", ""
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		switch {
		case request.Method == http.MethodGet && request.URL.Path == "/repos/owner/repo":
			writeManagedRepository(writer)
		case request.Method == http.MethodGet && request.URL.Path == "/repos/owner/repo/git/ref/heads/hive/repair":
			writeManagedRef(writer, "hive/repair", head)
		case request.Method == http.MethodGet && request.URL.Path == "/repos/owner/repo/pulls":
			listCalls++
			if createCalls == 0 {
				_, _ = io.WriteString(writer, `[]`)
			} else {
				writeManagedPullList(writer, managedPull(7, title, body, "hive/repair", head, "main", 123, "owner/repo"))
			}
		case request.Method == http.MethodPost && request.URL.Path == "/repos/owner/repo/pulls":
			createCalls++
			var input struct {
				Title string `json:"title"`
				Body  string `json:"body"`
			}
			_ = json.NewDecoder(request.Body).Decode(&input)
			title, body = input.Title, input.Body
			_, _ = io.WriteString(writer, `{"number":7}`)
		case request.Method == http.MethodGet && request.URL.Path == "/repos/owner/repo/pulls/7":
			writeManagedPull(writer, managedPull(7, title, body, "hive/repair", head, "main", 123, "owner/repo"))
		case request.Method == http.MethodPatch && request.URL.Path == "/repos/owner/repo/pulls/7":
			editCalls++
			var input struct {
				Title string `json:"title"`
				Body  string `json:"body"`
			}
			_ = json.NewDecoder(request.Body).Decode(&input)
			title, body = input.Title, input.Body
			writeManagedPull(writer, managedPull(7, title, body, "hive/repair", head, "main", 123, "owner/repo"))
		default:
			http.Error(writer, request.Method+" "+request.URL.Path, http.StatusNotFound)
		}
	}))
	defer server.Close()
	client := NewClientForTest(server.URL, "owner", []string{"repo"}, slog.Default())
	first, err := client.UpsertRepairPullRequest(context.Background(), "owner/repo", "hive/repair", head, "main", "Fix", marker+"\nRefs #3", marker)
	if err != nil || !first.Created || first.Number != 7 {
		t.Fatalf("first upsert = %+v, %v", first, err)
	}
	second, err := client.UpsertRepairPullRequest(context.Background(), "owner/repo", "hive/repair", head, "main", "Fix again", marker, marker)
	if err != nil || second.Created || second.HeadSHA != head {
		t.Fatalf("second upsert = %+v, %v", second, err)
	}
	if listCalls != 2 || createCalls != 1 || editCalls != 1 {
		t.Fatalf("unexpected calls list=%d create=%d edit=%d", listCalls, createCalls, editCalls)
	}
}

func TestUpsertRepairPullRequestRefreshesStaleListHeadAfterManagedPush(t *testing.T) {
	marker := "<!-- hive-setup: owner/repo -->"
	previousHead := strings.Repeat("9", 40)
	expectedHead := strings.Repeat("a", 40)
	title, body := "Previous setup", marker+"\nprevious"
	getCalls, editCalls, createCalls := 0, 0, 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		switch {
		case request.Method == http.MethodGet && request.URL.Path == "/repos/owner/repo":
			writeManagedRepository(writer)
		case request.Method == http.MethodGet && request.URL.Path == "/repos/owner/repo/git/ref/heads/hive/setup-proof":
			writeManagedRef(writer, "hive/setup-proof", expectedHead)
		case request.Method == http.MethodGet && request.URL.Path == "/repos/owner/repo/pulls":
			writeManagedPullList(writer, managedPull(7, title, body, "hive/setup-proof", previousHead, "main", 123, "owner/repo"))
		case request.Method == http.MethodGet && request.URL.Path == "/repos/owner/repo/pulls/7":
			getCalls++
			writeManagedPull(writer, managedPull(7, title, body, "hive/setup-proof", expectedHead, "main", 123, "owner/repo"))
		case request.Method == http.MethodPatch && request.URL.Path == "/repos/owner/repo/pulls/7":
			editCalls++
			var input struct {
				Title string `json:"title"`
				Body  string `json:"body"`
			}
			_ = json.NewDecoder(request.Body).Decode(&input)
			title, body = input.Title, input.Body
			writeManagedPull(writer, managedPull(7, title, body, "hive/setup-proof", expectedHead, "main", 123, "owner/repo"))
		case request.Method == http.MethodPost && request.URL.Path == "/repos/owner/repo/pulls":
			createCalls++
			http.Error(writer, "must not create a duplicate", http.StatusInternalServerError)
		default:
			http.Error(writer, request.Method+" "+request.URL.Path, http.StatusNotFound)
		}
	}))
	defer server.Close()
	client := NewClientForTest(server.URL, "owner", []string{"repo"}, slog.Default())
	pull, err := client.UpsertRepairPullRequest(context.Background(), "owner/repo", "hive/setup-proof", expectedHead, "main", "Current setup", marker+"\ncurrent", marker)
	if err != nil || pull.Created || pull.Number != 7 || pull.HeadSHA != expectedHead || getCalls != 3 || editCalls != 1 || createCalls != 0 {
		t.Fatalf("stale list head was not refreshed safely: pull=%+v gets=%d edits=%d creates=%d err=%v", pull, getCalls, editCalls, createCalls, err)
	}
}

func TestUpsertRepairPullRequestWaitsForManagedBranchRefAfterPush(t *testing.T) {
	marker := "<!-- hive-setup: owner/repo -->"
	priorHead := strings.Repeat("9", 40)
	expectedHead := strings.Repeat("a", 40)
	title, body := "Previous setup", marker+"\nprevious"
	refCalls, editCalls, createCalls := 0, 0, 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		switch {
		case request.Method == http.MethodGet && request.URL.Path == "/repos/owner/repo":
			writeManagedRepository(writer)
		case request.Method == http.MethodGet && request.URL.Path == "/repos/owner/repo/git/ref/heads/hive/setup-proof":
			refCalls++
			head := expectedHead
			if refCalls <= 2 {
				head = priorHead
			}
			writeManagedRef(writer, "hive/setup-proof", head)
		case request.Method == http.MethodGet && request.URL.Path == "/repos/owner/repo/pulls":
			writeManagedPullList(writer, managedPull(7, title, body, "hive/setup-proof", expectedHead, "main", 123, "owner/repo"))
		case request.Method == http.MethodGet && request.URL.Path == "/repos/owner/repo/pulls/7":
			writeManagedPull(writer, managedPull(7, title, body, "hive/setup-proof", expectedHead, "main", 123, "owner/repo"))
		case request.Method == http.MethodPatch && request.URL.Path == "/repos/owner/repo/pulls/7":
			editCalls++
			var input struct {
				Title string `json:"title"`
				Body  string `json:"body"`
			}
			_ = json.NewDecoder(request.Body).Decode(&input)
			title, body = input.Title, input.Body
			writeManagedPull(writer, managedPull(7, title, body, "hive/setup-proof", expectedHead, "main", 123, "owner/repo"))
		case request.Method == http.MethodPost && request.URL.Path == "/repos/owner/repo/pulls":
			createCalls++
			http.Error(writer, "must not create a duplicate", http.StatusInternalServerError)
		default:
			http.Error(writer, request.Method+" "+request.URL.Path, http.StatusNotFound)
		}
	}))
	defer server.Close()
	client := NewClientForTest(server.URL, "owner", []string{"repo"}, slog.Default())
	pull, err := client.UpsertRepairPullRequest(context.Background(), "owner/repo", "hive/setup-proof", expectedHead, "main", "Current setup", marker+"\ncurrent", marker)
	if err != nil || pull.Created || pull.Number != 7 || pull.HeadSHA != expectedHead || refCalls != 4 || editCalls != 1 || createCalls != 0 {
		t.Fatalf("managed branch ref did not converge safely: pull=%+v refs=%d edits=%d creates=%d err=%v", pull, refCalls, editCalls, createCalls, err)
	}
}

func TestWaitForManagedBranchHeadRetriesNewRefNotFound(t *testing.T) {
	expectedHead := strings.Repeat("a", 40)
	refCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		if request.Method != http.MethodGet || request.URL.Path != "/repos/owner/repo/git/ref/heads/hive/setup-new" {
			http.Error(writer, request.Method+" "+request.URL.Path, http.StatusNotFound)
			return
		}
		refCalls++
		if refCalls <= 2 {
			http.Error(writer, `{"message":"Not Found"}`, http.StatusNotFound)
			return
		}
		writeManagedRef(writer, "hive/setup-new", expectedHead)
	}))
	defer server.Close()

	client := NewClientForTest(server.URL, "owner", []string{"repo"}, slog.Default())
	if err := client.waitForManagedBranchHead(context.Background(), "owner", "repo", "hive/setup-new", expectedHead); err != nil {
		t.Fatalf("new managed branch did not converge after transient 404: %v", err)
	}
	if refCalls != 3 {
		t.Fatalf("ref calls = %d, want 3", refCalls)
	}
}

func TestWaitForManagedBranchHeadDoesNotRetryForbidden(t *testing.T) {
	refCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		refCalls++
		http.Error(writer, `{"message":"Forbidden"}`, http.StatusForbidden)
	}))
	defer server.Close()

	client := NewClientForTest(server.URL, "owner", []string{"repo"}, slog.Default())
	err := client.waitForManagedBranchHead(context.Background(), "owner", "repo", "hive/setup-new", strings.Repeat("a", 40))
	if err == nil || !strings.Contains(err.Error(), "verify managed branch") {
		t.Fatalf("forbidden branch lookup = %v, want immediate verification failure", err)
	}
	if refCalls != 1 {
		t.Fatalf("forbidden ref calls = %d, want 1", refCalls)
	}
}

func TestUpsertSetupPullRequestWaitsForExactStateBoundPriorHead(t *testing.T) {
	marker := "<!-- hive-setup: owner/repo -->"
	priorHead := strings.Repeat("9", 40)
	expectedHead := strings.Repeat("a", 40)
	title, body := "Previous setup", marker+"\nprevious"
	getCalls, editCalls, createCalls := 0, 0, 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		switch {
		case request.Method == http.MethodGet && request.URL.Path == "/repos/owner/repo":
			writeManagedRepository(writer)
		case request.Method == http.MethodGet && request.URL.Path == "/repos/owner/repo/git/ref/heads/hive/setup-proof":
			writeManagedRef(writer, "hive/setup-proof", expectedHead)
		case request.Method == http.MethodGet && request.URL.Path == "/repos/owner/repo/pulls":
			writeManagedPullList(writer, managedPull(7, title, body, "hive/setup-proof", priorHead, "main", 123, "owner/repo"))
		case request.Method == http.MethodGet && request.URL.Path == "/repos/owner/repo/pulls/7":
			getCalls++
			head := expectedHead
			if getCalls <= 2 {
				head = priorHead
			}
			writeManagedPull(writer, managedPull(7, title, body, "hive/setup-proof", head, "main", 123, "owner/repo"))
		case request.Method == http.MethodPatch && request.URL.Path == "/repos/owner/repo/pulls/7":
			editCalls++
			var input struct {
				Title string `json:"title"`
				Body  string `json:"body"`
			}
			_ = json.NewDecoder(request.Body).Decode(&input)
			title, body = input.Title, input.Body
			writeManagedPull(writer, managedPull(7, title, body, "hive/setup-proof", expectedHead, "main", 123, "owner/repo"))
		case request.Method == http.MethodPost && request.URL.Path == "/repos/owner/repo/pulls":
			createCalls++
			http.Error(writer, "must not create a duplicate", http.StatusInternalServerError)
		default:
			http.Error(writer, request.Method+" "+request.URL.Path, http.StatusNotFound)
		}
	}))
	defer server.Close()
	client := NewClientForTest(server.URL, "owner", []string{"repo"}, slog.Default())
	pull, err := client.UpsertSetupPullRequest(context.Background(), "owner/repo", "hive/setup-proof", expectedHead, "main", "Current setup", marker+"\ncurrent", marker, 7, priorHead)
	if err != nil || pull.Created || pull.Number != 7 || pull.HeadSHA != expectedHead || getCalls != 5 || editCalls != 1 || createCalls != 0 {
		t.Fatalf("state-bound setup head did not converge safely: pull=%+v gets=%d edits=%d creates=%d err=%v", pull, getCalls, editCalls, createCalls, err)
	}
}

func TestUpsertSetupPullRequestRejectsUnboundPriorHead(t *testing.T) {
	marker := "<!-- hive-setup: owner/repo -->"
	liveHead := strings.Repeat("9", 40)
	expectedHead := strings.Repeat("a", 40)
	mutations := 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		switch {
		case request.Method == http.MethodGet && request.URL.Path == "/repos/owner/repo":
			writeManagedRepository(writer)
		case request.Method == http.MethodGet && request.URL.Path == "/repos/owner/repo/git/ref/heads/hive/setup-proof":
			writeManagedRef(writer, "hive/setup-proof", expectedHead)
		case request.Method == http.MethodGet && request.URL.Path == "/repos/owner/repo/pulls":
			writeManagedPullList(writer, managedPull(7, "Previous setup", marker, "hive/setup-proof", liveHead, "main", 123, "owner/repo"))
		case request.Method == http.MethodGet && request.URL.Path == "/repos/owner/repo/pulls/7":
			writeManagedPull(writer, managedPull(7, "Previous setup", marker, "hive/setup-proof", liveHead, "main", 123, "owner/repo"))
		default:
			mutations++
			http.Error(writer, "must not mutate", http.StatusInternalServerError)
		}
	}))
	defer server.Close()
	client := NewClientForTest(server.URL, "owner", []string{"repo"}, slog.Default())
	_, err := client.UpsertSetupPullRequest(context.Background(), "owner/repo", "hive/setup-proof", expectedHead, "main", "Current setup", marker, marker, 7, strings.Repeat("8", 40))
	if err == nil || !strings.Contains(err.Error(), "preclaims") || mutations != 0 {
		t.Fatalf("unbound prior setup head was not rejected: mutations=%d err=%v", mutations, err)
	}
}

func TestUpsertReviewPullRequestIsDraftAndHoldLabeled(t *testing.T) {
	head := strings.Repeat("b", 40)
	marker := "<!-- hive-baseline-review: proof -->"
	title, body := "", ""
	draft := false
	labelsAdded := []string{}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		switch {
		case request.Method == http.MethodGet && request.URL.Path == "/repos/owner/repo":
			writeManagedRepository(writer)
		case request.Method == http.MethodGet && request.URL.Path == "/repos/owner/repo/git/ref/heads/hive/baseline":
			writeManagedRef(writer, "hive/baseline", head)
		case request.Method == http.MethodGet && request.URL.Path == "/repos/owner/repo/pulls":
			_, _ = io.WriteString(writer, `[]`)
		case request.Method == http.MethodPost && request.URL.Path == "/repos/owner/repo/pulls":
			var input struct {
				Draft bool   `json:"draft"`
				Title string `json:"title"`
				Body  string `json:"body"`
			}
			_ = json.NewDecoder(request.Body).Decode(&input)
			draft, title, body = input.Draft, input.Title, input.Body
			_, _ = io.WriteString(writer, `{"number":7}`)
		case request.Method == http.MethodGet && request.URL.Path == "/repos/owner/repo/pulls/7":
			writeManagedPull(writer, managedPull(7, title, body, "hive/baseline", head, "main", 123, "owner/repo"))
		case request.Method == http.MethodPost && request.URL.Path == "/repos/owner/repo/labels":
			_, _ = io.WriteString(writer, `{"name":"created"}`)
		case request.Method == http.MethodPost && request.URL.Path == "/repos/owner/repo/issues/7/labels":
			_ = json.NewDecoder(request.Body).Decode(&labelsAdded)
			_, _ = io.WriteString(writer, `[]`)
		default:
			http.Error(writer, request.Method+" "+request.URL.Path, http.StatusNotFound)
		}
	}))
	defer server.Close()
	client := NewClientForTest(server.URL, "owner", []string{"repo"}, slog.Default())
	pull, err := client.UpsertReviewPullRequest(context.Background(), "owner/repo", "hive/baseline", head, "main", "Review baseline", marker+"\nbody", marker)
	if err != nil {
		t.Fatal(err)
	}
	if !draft || pull.Number != 7 || len(labelsAdded) != 2 || labelsAdded[0] != "hold" || labelsAdded[1] != "hive/baseline-review" {
		t.Fatalf("review PR was not held: draft=%t pull=%+v labels=%v", draft, pull, labelsAdded)
	}
}

func TestUpsertRepairPullRequestRejectsDuplicateMarker(t *testing.T) {
	marker := "<!-- hive-repair: duplicate -->"
	head := strings.Repeat("c", 40)
	mutations := 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		switch {
		case request.Method == http.MethodGet && request.URL.Path == "/repos/owner/repo":
			writeManagedRepository(writer)
		case request.Method == http.MethodGet && request.URL.Path == "/repos/owner/repo/git/ref/heads/branch":
			writeManagedRef(writer, "branch", head)
		case request.Method == http.MethodGet && request.URL.Path == "/repos/owner/repo/pulls":
			writeManagedPullList(writer,
				managedPull(1, "title", marker, "branch", head, "main", 123, "owner/repo"),
				managedPull(2, "title", marker, "branch", head, "main", 123, "owner/repo"),
			)
		default:
			mutations++
			http.Error(writer, "must not mutate", http.StatusInternalServerError)
		}
	}))
	defer server.Close()
	client := NewClientForTest(server.URL, "owner", []string{"repo"}, slog.Default())
	_, err := client.UpsertRepairPullRequest(context.Background(), "owner/repo", "branch", head, "main", "title", marker, marker)
	if err == nil || !strings.Contains(err.Error(), "multiple exact") || mutations != 0 {
		t.Fatalf("expected duplicate rejection, got %v", err)
	}
}

func TestUpsertRepairPullRequestRejectsMarkerOnAnotherBranch(t *testing.T) {
	marker := "<!-- hive-repair: stable -->"
	head := strings.Repeat("d", 40)
	createCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		switch {
		case request.Method == http.MethodGet && request.URL.Path == "/repos/owner/repo":
			writeManagedRepository(writer)
		case request.Method == http.MethodGet && request.URL.Path == "/repos/owner/repo/git/ref/heads/hive/repair-retry":
			writeManagedRef(writer, "hive/repair-retry", head)
		case request.Method == http.MethodGet && request.URL.Path == "/repos/owner/repo/pulls":
			writeManagedPullList(writer, managedPull(7, "title", marker, "hive/repair-original", head, "main", 123, "owner/repo"))
		case request.Method == http.MethodGet && request.URL.Path == "/repos/owner/repo/pulls/7":
			writeManagedPull(writer, managedPull(7, "title", marker, "hive/repair-original", head, "main", 123, "owner/repo"))
		default:
			createCalls++
			http.Error(writer, "must not create", http.StatusInternalServerError)
		}
	}))
	defer server.Close()
	client := NewClientForTest(server.URL, "owner", []string{"repo"}, slog.Default())
	_, err := client.UpsertRepairPullRequest(context.Background(), "owner/repo", "hive/repair-retry", head, "main", "title", marker, marker)
	if err == nil || !strings.Contains(err.Error(), "preclaims") || createCalls != 0 {
		t.Fatalf("cross-branch duplicate was not rejected: calls=%d err=%v", createCalls, err)
	}
}

func TestUpsertRepairPullRequestRejectsTargetRepositoryClaimWithWrongHeadOrBase(t *testing.T) {
	marker := "<!-- hive-repair: exact -->"
	expectedHead := strings.Repeat("a", 40)
	for _, test := range []struct {
		name string
		head string
		base string
	}{
		{name: "wrong head", head: strings.Repeat("b", 40), base: "main"},
		{name: "wrong base", head: expectedHead, base: "release"},
	} {
		t.Run(test.name, func(t *testing.T) {
			mutations := 0
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				writer.Header().Set("Content-Type", "application/json")
				switch {
				case request.Method == http.MethodGet && request.URL.Path == "/repos/owner/repo":
					writeManagedRepository(writer)
				case request.Method == http.MethodGet && request.URL.Path == "/repos/owner/repo/git/ref/heads/hive/repair-exact":
					writeManagedRef(writer, "hive/repair-exact", expectedHead)
				case request.Method == http.MethodGet && request.URL.Path == "/repos/owner/repo/pulls":
					writeManagedPullList(writer, managedPull(7, "title", marker, "hive/repair-exact", test.head, test.base, 123, "owner/repo"))
				case request.Method == http.MethodGet && request.URL.Path == "/repos/owner/repo/pulls/7":
					writeManagedPull(writer, managedPull(7, "title", marker, "hive/repair-exact", test.head, test.base, 123, "owner/repo"))
				default:
					mutations++
					http.Error(writer, "must not mutate", http.StatusInternalServerError)
				}
			}))
			defer server.Close()
			client := NewClientForTest(server.URL, "owner", []string{"repo"}, slog.Default())
			_, err := client.UpsertRepairPullRequest(context.Background(), "owner/repo", "hive/repair-exact", expectedHead, "main", "title", marker, marker)
			if err == nil || !strings.Contains(err.Error(), "preclaims") || mutations != 0 {
				t.Fatalf("inexact target-repository claim was not rejected: mutations=%d err=%v", mutations, err)
			}
		})
	}
}

func TestUpsertRepairPullRequestIgnoresForkMarkerPreclaim(t *testing.T) {
	marker := "<!-- hive-setup: owner/repo -->"
	head := strings.Repeat("e", 40)
	createCalls, editCalls := 0, 0
	title, body := "Install", marker+"\nmanaged"
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		switch {
		case request.Method == http.MethodGet && request.URL.Path == "/repos/owner/repo":
			writeManagedRepository(writer)
		case request.Method == http.MethodGet && request.URL.Path == "/repos/owner/repo/git/ref/heads/hive/setup-proof":
			writeManagedRef(writer, "hive/setup-proof", head)
		case request.Method == http.MethodGet && request.URL.Path == "/repos/owner/repo/pulls":
			writeManagedPullList(writer, managedPull(7, "spoof", marker, "hive/setup-proof", head, "main", 999, "attacker/repo"))
		case request.Method == http.MethodPost && request.URL.Path == "/repos/owner/repo/pulls":
			createCalls++
			_, _ = io.WriteString(writer, `{"number":8}`)
		case request.Method == http.MethodGet && request.URL.Path == "/repos/owner/repo/pulls/8":
			writeManagedPull(writer, managedPull(8, title, body, "hive/setup-proof", head, "main", 123, "owner/repo"))
		case request.Method == http.MethodPatch:
			editCalls++
			http.Error(writer, "must not edit fork claimant", http.StatusInternalServerError)
		default:
			http.Error(writer, request.Method+" "+request.URL.Path, http.StatusNotFound)
		}
	}))
	defer server.Close()
	client := NewClientForTest(server.URL, "owner", []string{"repo"}, slog.Default())
	pull, err := client.UpsertRepairPullRequest(context.Background(), "owner/repo", "hive/setup-proof", head, "main", title, body, marker)
	if err != nil || pull.Number != 8 || !pull.Created || createCalls != 1 || editCalls != 0 {
		t.Fatalf("fork preclaim influenced managed upsert: pull=%+v creates=%d edits=%d err=%v", pull, createCalls, editCalls, err)
	}
}

func TestListOpenRepairPullRequestsFiltersForkMarkerPreclaim(t *testing.T) {
	marker := "<!-- hive-repair: proof -->"
	head := strings.Repeat("f", 40)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		switch {
		case request.Method == http.MethodGet && request.URL.Path == "/repos/owner/repo":
			writeManagedRepository(writer)
		case request.Method == http.MethodGet && request.URL.Path == "/repos/owner/repo/pulls":
			writeManagedPullList(writer,
				managedPull(10, "spoof", marker, "hive/repair-proof", head, "main", 999, "attacker/repo"),
				managedPull(11, "managed", marker, "hive/repair-proof", head, "main", 123, "owner/repo"),
			)
		case request.Method == http.MethodGet && request.URL.Path == "/repos/owner/repo/git/ref/heads/hive/repair-proof":
			writeManagedRef(writer, "hive/repair-proof", head)
		default:
			http.Error(writer, request.Method+" "+request.URL.Path, http.StatusNotFound)
		}
	}))
	defer server.Close()
	client := NewClientForTest(server.URL, "owner", []string{"repo"}, slog.Default())
	pulls, err := client.ListOpenRepairPullRequests(context.Background(), "owner/repo", "main", marker)
	if err != nil || len(pulls) != 1 || pulls[0].Number != 11 {
		t.Fatalf("foreign marker claimant entered duplicate reconciliation: pulls=%+v err=%v", pulls, err)
	}
}

func TestCloseRepairPullRequestExactRejectsFork(t *testing.T) {
	marker := "<!-- hive-repair: proof -->"
	head := strings.Repeat("1", 40)
	patchCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		switch {
		case request.Method == http.MethodGet && request.URL.Path == "/repos/owner/repo":
			writeManagedRepository(writer)
		case request.Method == http.MethodGet && request.URL.Path == "/repos/owner/repo/pulls/9":
			writeManagedPull(writer, managedPull(9, "spoof", marker, "hive/repair-proof", head, "main", 999, "attacker/repo"))
		case request.Method == http.MethodPatch:
			patchCalls++
			http.Error(writer, "must not close fork", http.StatusInternalServerError)
		default:
			http.Error(writer, request.Method+" "+request.URL.Path, http.StatusNotFound)
		}
	}))
	defer server.Close()
	client := NewClientForTest(server.URL, "owner", []string{"repo"}, slog.Default())
	err := client.CloseRepairPullRequestExact(context.Background(), "owner/repo", 9, marker, "hive/repair-proof", head)
	if err == nil || !strings.Contains(err.Error(), "head and base repositories") || patchCalls != 0 {
		t.Fatalf("foreign pull request close was not rejected: patch=%d err=%v", patchCalls, err)
	}
}

func TestListMergedBaselineBranchesRequiresLiveExactLabeledMerge(t *testing.T) {
	fingerprint := strings.Repeat("a", 64)
	repairHead := strings.Repeat("b", 40)
	marker := fmt.Sprintf("<!-- hive-baseline-review: %s:%s -->", fingerprint, repairHead)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		switch {
		case request.Method == http.MethodGet && request.URL.Path == "/repos/owner/repo":
			writeManagedRepository(writer)
		case request.Method == http.MethodGet && request.URL.Path == "/repos/owner/repo/git/matching-refs/heads/hive/baseline-":
			_, _ = io.WriteString(writer, `[{"ref":"refs/heads/hive/baseline-proof","object":{"sha":"baseline-head","type":"commit"}}]`)
		case request.Method == http.MethodGet && request.URL.Path == "/repos/owner/repo/pulls" && request.URL.Query().Get("state") == "closed":
			foreign := managedPull(136, "spoof", marker, "hive/baseline-proof", "baseline-head", "main", 999, "attacker/repo")
			foreign["labels"] = []map[string]any{{"name": "hive/baseline-review"}}
			valid := managedPull(137, "review", marker, "hive/baseline-proof", "baseline-head", "main", 123, "owner/repo")
			valid["labels"] = []map[string]any{{"name": "hive/baseline-review"}}
			writeManagedPullList(writer, foreign, valid)
		case request.Method == http.MethodGet && request.URL.Path == "/repos/owner/repo/pulls/137":
			valid := managedPull(137, "review", marker, "hive/baseline-proof", "baseline-head", "main", 123, "owner/repo")
			valid["state"], valid["merged"], valid["labels"] = "closed", true, []map[string]any{{"name": "hive/baseline-review"}}
			writeManagedPull(writer, valid)
		default:
			http.Error(writer, request.Method+" "+request.URL.Path, http.StatusNotFound)
		}
	}))
	defer server.Close()
	client := NewClientForTest(server.URL, "owner", []string{"repo"}, slog.Default())
	branches, err := client.ListMergedBaselineBranches(context.Background(), "owner/repo", "main")
	if err != nil {
		t.Fatal(err)
	}
	if len(branches) != 1 || branches[0].PRNumber != 137 || branches[0].RepositoryFingerprint != fingerprint || branches[0].Branch != "hive/baseline-proof" || branches[0].HeadSHA != "baseline-head" {
		t.Fatalf("unexpected merged baseline branches: %+v", branches)
	}
}

func managedPull(number int, title, body, branch, head, base string, headRepositoryID int64, headRepository string) map[string]any {
	return map[string]any{
		"number": number, "html_url": fmt.Sprintf("https://example.test/pull/%d", number), "state": "open", "title": title, "body": body,
		"head": map[string]any{"ref": branch, "sha": head, "repo": map[string]any{"id": headRepositoryID, "full_name": headRepository}},
		"base": map[string]any{"ref": base, "sha": strings.Repeat("2", 40), "repo": map[string]any{"id": int64(123), "full_name": "owner/repo"}},
	}
}

func writeManagedRepository(writer http.ResponseWriter) {
	_ = json.NewEncoder(writer).Encode(map[string]any{"id": int64(123), "full_name": "owner/repo"})
}

func writeManagedRef(writer http.ResponseWriter, branch, head string) {
	_ = json.NewEncoder(writer).Encode(map[string]any{"ref": "refs/heads/" + branch, "object": map[string]any{"sha": head, "type": "commit"}})
}

func writeManagedPull(writer http.ResponseWriter, pull map[string]any) {
	_ = json.NewEncoder(writer).Encode(pull)
}

func writeManagedPullList(writer http.ResponseWriter, pulls ...map[string]any) {
	_ = json.NewEncoder(writer).Encode(pulls)
}

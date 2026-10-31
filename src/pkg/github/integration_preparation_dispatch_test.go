package github

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	gh "github.com/google/go-github/v72/github"
	"github.com/hivecommons/hive/pkg/effects"
	"github.com/hivecommons/hive/pkg/forgestate"
)

type integrationPreparationTestTransport func(*http.Request) (*http.Response, error)

func (transport integrationPreparationTestTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	return transport(request)
}

func TestIntegrationPreparationJournalsExactPublicTransportWithoutBlindRetry(t *testing.T) {
	request := IntegrationPreparationRequest{
		Schema: 1, Protocol: "integration-prefix-v1", Engine: "hive", Repo: "o/r",
		Base: "v5", BaseSHA: strings.Repeat("a", 40), Policy: strings.Repeat("b", 64),
		Generation: 1, CreatedAt: "2026-10-31T03:00:00Z",
		Members: []forgestate.IntegrationCandidateMember{{PR: 1, Head: strings.Repeat("c", 40)}},
	}
	raw, digest, err := EncodeIntegrationPreparation(request)
	if err != nil {
		t.Fatal(err)
	}
	for _, scenario := range []struct {
		name        string
		status      int
		refused     bool
		transport   bool
		guardRefuse bool
	}{
		{"accepted", http.StatusNoContent, false, false, false},
		{"forbidden", http.StatusForbidden, true, false, false},
		{"provider-error", http.StatusInternalServerError, false, false, false},
		{"ambiguous-success", http.StatusOK, false, false, false},
		{"transport-error", 0, false, true, false},
		{"guard-refusal", 0, true, false, true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			journal := new(integrationPromotionJournal)
			calls := 0
			transport := integrationPreparationTestTransport(func(r *http.Request) (*http.Response, error) {
				calls++
				if journal.claim.Kind != integrationPreparationEffectKind || journal.claim.Inputs["request_digest"] != digest {
					t.Fatal("provider RPC ran before exact journal intent")
				}
				if r.Method != http.MethodPost || r.URL.String() != "https://api.github.com/repos/o/r/dispatches" {
					t.Fatalf("unexpected preparation transport: %s %s", r.Method, r.URL)
				}
				var body struct {
					Event   string            `json:"event_type"`
					Payload map[string]string `json:"client_payload"`
				}
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Event != "hive.integration.prepare.v1" ||
					body.Payload["request"] != raw || body.Payload["request_key"] != digest {
					t.Fatalf("transport changed exact subject: %+v %v", body, err)
				}
				if scenario.transport {
					return nil, errors.New("connection lost after possible acceptance")
				}
				return &http.Response{StatusCode: scenario.status, Header: make(http.Header), Request: r,
					Body: io.NopCloser(strings.NewReader(`{"message":"controlled fixture"}`))}, nil
			})
			client := &Client{client: gh.NewClient(&http.Client{Transport: transport})}
			guard := func(context.Context) error {
				if scenario.guardRefuse {
					return errors.New("authority revoked")
				}
				return nil
			}
			result, err := client.DispatchIntegrationPreparation(context.Background(), request, raw, digest,
				strings.Repeat("d", 64), strings.Repeat("e", 40), journal, guard)
			if scenario.name == "accepted" {
				if err != nil || result.Provenance != "repository-dispatch-accepted/"+digest {
					t.Fatalf("exact accepted transport refused: %+v %v", result, err)
				}
			} else if err == nil || errors.Is(err, effects.ErrNotApplied) != scenario.refused {
				t.Fatalf("incorrect uncertainty classification: %+v %v", result, err)
			}
			if scenario.guardRefuse && calls != 0 {
				t.Fatal("revoked authority reached provider")
			}
			if !scenario.refused {
				_, _ = client.DispatchIntegrationPreparation(context.Background(), request, raw, digest,
					strings.Repeat("d", 64), strings.Repeat("e", 40), journal, guard)
				if calls != 1 {
					t.Fatal("accepted/ambiguous dispatch was repeated without owner reconciliation")
				}
			}
		})
	}
}

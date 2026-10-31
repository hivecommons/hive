package github

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/effects"
)

func TestIntegrationPromotionAuthorityEpochIsInAtomicMutation(t *testing.T) {
	for _, moved := range []bool{false, true} {
		t.Run(map[bool]string{false: "stable", true: "changed-after-guard"}[moved], func(t *testing.T) {
			f := promotionFixture()
			f.request.AuthorityRef = "refs/heads/hive-integration-authority/v5"
			f.request.AuthorityCommit = strings.Repeat("e", 40)
			f.authority = f.request.AuthorityCommit
			server := f.server(t)
			defer server.Close()
			client := testClient(t, server.URL)
			journal := new(integrationPromotionJournal)
			guard := func(context.Context, IntegrationPromotion) error {
				if moved {
					f.mu.Lock()
					f.authority = strings.Repeat("f", 40)
					f.mu.Unlock()
				}
				return nil
			}
			_, acknowledged, err := client.PromoteIntegrationStepWithAcknowledgement(context.Background(), f.request, journal, guard)
			if (err != nil) != moved {
				t.Fatalf("moved=%t: %v", moved, err)
			}
			if acknowledged == moved {
				t.Fatal("provider acknowledgement did not distinguish an actual applied RPC from refusal")
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			if moved && f.ref != f.request.ExpectedSHA {
				t.Fatal("an authority change advanced the destination")
			}
			if !moved && f.ref != f.request.TestedSHA {
				t.Fatal("stable exact promotion did not advance")
			}
			updates, ok := f.input["refUpdates"].([]any)
			if !ok || len(updates) != 3 {
				t.Fatalf("missing third atomic fence: %#v", f.input)
			}
			epoch, ok := updates[2].(map[string]any)
			if !ok || epoch["name"] != f.request.AuthorityRef ||
				epoch["beforeOid"] != f.request.AuthorityCommit || epoch["afterOid"] != f.request.AuthorityCommit ||
				epoch["force"] != false || journal.claim.Inputs["authority_commit"] != f.request.AuthorityCommit {
				t.Fatalf("authority epoch missing from CAS/journal: %#v %+v", epoch, journal.claim)
			}
		})
	}
}

func TestIntegrationPromotionRejectsIncompleteAuthorityFence(t *testing.T) {
	for _, change := range []func(*IntegrationPromotion){
		func(p *IntegrationPromotion) { p.AuthorityRef = "refs/heads/hive-integration-authority/v5" },
		func(p *IntegrationPromotion) { p.AuthorityCommit = strings.Repeat("e", 40) },
		func(p *IntegrationPromotion) {
			p.AuthorityRef = "refs/heads/v5"
			p.AuthorityCommit = strings.Repeat("e", 40)
		},
		func(p *IntegrationPromotion) {
			p.AuthorityRef = "refs/heads/hive-integration-authority/../v5"
			p.AuthorityCommit = strings.Repeat("e", 40)
		},
	} {
		request := integrationPromotionRequest()
		change(&request)
		if request.validate() == nil {
			t.Fatalf("accepted unsafe epoch: %+v", request)
		}

	}
}

func TestIntegrationReconciledDestinationCannotQualifyUnprovenActor(t *testing.T) {
	fixture := promotionFixture()
	fixture.ref = fixture.request.TestedSHA
	server := fixture.server(t)
	defer server.Close()
	client := testClient(t, server.URL)
	journal := &integrationPromotionJournal{unresolved: true}
	result, acknowledged, err := client.PromoteIntegrationStepWithAcknowledgement(context.Background(), fixture.request, journal,
		func(context.Context, IntegrationPromotion) error {
			t.Error("reconciled state must not issue another CAS")
			return nil
		})
	if err != nil || result.Provenance != fixture.request.TestedSHA || acknowledged || fixture.writes.Load() != 0 {
		t.Fatalf("reconciliation fabricated successful actor RPC: %+v ack=%v err=%v writes=%d", result, acknowledged, err, fixture.writes.Load())
	}
}

func TestIntegrationPreparationCanonicalGolden(t *testing.T) {
	request := IntegrationPreparationRequest{
		Schema: 1, Protocol: "integration-prefix-v1", Engine: "hive", Repo: "o/r",
		Base: "topic/雪\u2028<&", BaseSHA: strings.Repeat("a", 40), Policy: strings.Repeat("b", 64),
		Generation: 1, CreatedAt: "2026-10-31T03:00:00Z",
	}
	text, digest, err := EncodeIntegrationPreparation(request)
	expected := `{"base":"topic/雪` + "\u2028" + `<&","baseSHA":"` + request.BaseSHA +
		`","createdAt":"2026-10-31T03:00:00Z","engine":"hive","generation":1,"members":null,"policy":"` +
		request.Policy + `","protocol":"integration-prefix-v1","repo":"o/r","schema":1}`
	hash := sha256.Sum256([]byte(expected))
	if err != nil || text != expected || digest != hex.EncodeToString(hash[:]) {
		t.Fatalf("canonical Go/Node protocol mismatch: %q %s %v", text, digest, err)
	}
	request.Generation = 1 << 53
	if _, _, err := EncodeIntegrationPreparation(request); err == nil {
		t.Fatal("accepted a non-JavaScript-safe request generation")
	}
	request.Generation = -1
	if _, _, err := EncodeIntegrationPreparation(request); err == nil {
		t.Fatal("accepted negative protocol generation")
	}
	request.Generation, request.Base = 1, strings.Repeat("a", 33<<10)
	if _, _, err := EncodeIntegrationPreparation(request); err == nil {
		t.Fatal("accepted oversized request")
	}
}

func TestIntegrationPreparationRequiresExactTypedRequestBeforeEffect(t *testing.T) {
	request := IntegrationPreparationRequest{
		Schema: 1, Protocol: "integration-prefix-v1", Engine: "hive", Repo: "o/r", Base: "v5",
		BaseSHA: strings.Repeat("a", 40), Policy: strings.Repeat("b", 64),
		Generation: 1, CreatedAt: "2026-10-31T03:00:00Z",
	}
	client := testClient(t, "http://127.0.0.1:1")
	text, digest, err := EncodeIntegrationPreparation(request)
	if err != nil {
		t.Fatal(err)
	}
	for _, boundary := range []effects.Boundary{nil, effects.NoopBoundary{}, new(integrationPromotionJournal)} {
		if _, err := client.DispatchIntegrationPreparation(context.Background(), request, text, digest,
			strings.Repeat("c", 64), strings.Repeat("d", 40), boundary, nil); err == nil {
			t.Fatal("missing guard/boundary or empty members accepted")
		}
	}
}

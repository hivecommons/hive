package hub

import (
	"encoding/json"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/spoke"
)

// TestHeartbeatSigTagsMatch pins the sig_* json tags on HeartbeatResponse
// against the minimal envelope pkg/spoke uses to read the binding back. If a tag
// drifts on either side, a signed body would parse to a zero binding on the
// spoke and every enforcing spoke would reject every response — this test is the
// tripwire for that.
func TestHeartbeatSigTagsMatch(t *testing.T) {
	respType := reflect.TypeOf(HeartbeatResponse{})
	want := map[string]string{
		"SigHiveID":   "sig_hive_id",
		"SigSeq":      "sig_seq",
		"SigSignedAt": "sig_ts",
		"SigVersion":  "sig_v",
	}
	for field, tag := range want {
		f, ok := respType.FieldByName(field)
		if !ok {
			t.Fatalf("HeartbeatResponse missing field %s", field)
		}
		got := strings.Split(f.Tag.Get("json"), ",")[0]
		if got != tag {
			t.Errorf("HeartbeatResponse.%s json tag = %q, want %q", field, got, tag)
		}
	}
}

// TestHubSignsAndSpokeVerifies is the end-to-end: the hub signs a response with
// the master-derived SSO seed, and a pkg/spoke verifier holding the matching
// public key accepts it — proving the hub mint and spoke verify agree on the
// wire format and on the key (no new key introduced).
func TestHubSignsAndSpokeVerifies(t *testing.T) {
	const master = "test-master-secret-7082"
	s := &HubServer{hubSecret: master}
	pub := ssoPublicKeyFromSeed(deriveDomainKey(master, infoSSOEd25519Seed))

	rec := httptest.NewRecorder()
	resp := &HeartbeatResponse{OK: true}
	s.signHeartbeatResponse(rec, resp, "hive-a")

	if resp.SigHiveID != "hive-a" || resp.SigVersion != spoke.SigVersion || resp.SigSeq == 0 {
		t.Fatalf("hub did not fill binding: %+v", resp)
	}
	body, _ := json.Marshal(resp)
	sig := rec.Header().Get(spoke.SigHeader)
	if sig == "" {
		t.Fatal("hub set no signature header")
	}

	v := spoke.NewVerifier(spoke.ModeEnforce, nil)
	got := v.Verify([]string{pub}, "hive-a", body, sig, time.Now())
	if !got.Accepted || !got.Signed {
		t.Fatalf("spoke rejected a genuine hub-signed response: %+v", got)
	}

	// Cross-hive: same signed bytes presented to a spoke serving hive-b.
	vB := spoke.NewVerifier(spoke.ModeEnforce, nil)
	// Establish trust for hive-b first with its own signed response.
	recB := httptest.NewRecorder()
	respB := &HeartbeatResponse{OK: true}
	s.signHeartbeatResponse(recB, respB, "hive-b")
	bodyB, _ := json.Marshal(respB)
	vB.Verify([]string{pub}, "hive-b", bodyB, recB.Header().Get(spoke.SigHeader), time.Now())
	// Now the hive-a response must be rejected by hive-b.
	if got := vB.Verify([]string{pub}, "hive-b", body, sig, time.Now()); got.Accepted {
		t.Fatalf("cross-hive replay accepted by hive-b: %+v", got)
	}
}

// TestHubSeqMonotonic proves the hub hands out strictly increasing seqs per
// hive, so a spoke's rollback floor keeps advancing.
func TestHubSeqMonotonic(t *testing.T) {
	const master = "test-master-secret-7082-seq"
	s := &HubServer{hubSecret: master}
	var prev int64
	for i := 0; i < 5; i++ {
		rec := httptest.NewRecorder()
		resp := &HeartbeatResponse{OK: true}
		s.signHeartbeatResponse(rec, resp, "hive-seq")
		if resp.SigSeq <= prev {
			t.Fatalf("seq not strictly increasing: prev=%d got=%d", prev, resp.SigSeq)
		}
		prev = resp.SigSeq
	}
}

// TestKeylessHubEmitsUnsigned proves a hub with no secret leaves the response
// unsigned (legacy behaviour) rather than emitting a bogus signature — the
// no-bricking guarantee on the hub side.
func TestKeylessHubEmitsUnsigned(t *testing.T) {
	s := &HubServer{hubSecret: ""}
	rec := httptest.NewRecorder()
	resp := &HeartbeatResponse{OK: true}
	s.signHeartbeatResponse(rec, resp, "hive-a")
	if rec.Header().Get(spoke.SigHeader) != "" {
		t.Fatal("keyless hub must not set a signature header")
	}
	if resp.SigHiveID != "" || resp.SigSeq != 0 {
		t.Fatalf("keyless hub must not fill the binding: %+v", resp)
	}
}

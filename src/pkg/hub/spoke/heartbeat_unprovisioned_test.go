package spoke

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/fleetreport"
)

func TestPostHeartbeat_UnprovisionedForbiddenBacksOff(t *testing.T) {
	t.Cleanup(ResetHeartbeatStateForTest)
	ResetHeartbeatStateForTest()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "hosted/saas hive IDs can only be created via provisioning", http.StatusForbidden)
	}))
	defer srv.Close()

	if resp := postHeartbeatToHub(context.Background(), srv.URL, &HeartbeatPayload{}, nil2Logger()); resp != nil {
		t.Fatalf("rejected beat returned a response: %+v", resp)
	}
	if !heartbeatUnprovisionedBackoffActive(time.Now()) {
		t.Fatal("expected backoff after unprovisioned 403")
	}
	if heartbeatUnprovisionedBackoffActive(time.Now().Add(heartbeatUnprovisionedBackoff + time.Second)) {
		t.Fatal("backoff must expire")
	}
}

func TestPostHeartbeat_OtherForbiddenDoesNotBackOff(t *testing.T) {
	t.Cleanup(ResetHeartbeatStateForTest)
	ResetHeartbeatStateForTest()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "bad secret", http.StatusForbidden)
	}))
	defer srv.Close()

	postHeartbeatToHub(context.Background(), srv.URL, &HeartbeatPayload{}, nil2Logger())
	if heartbeatUnprovisionedBackoffActive(time.Now()) {
		t.Fatal("a 403 without the provisioning body must not back off")
	}
}

func TestHubHeartbeatRejectsUnprovisioned(t *testing.T) {
	body := "hosted/saas hive IDs can only be created via provisioning"
	if !hubHeartbeatRejectsUnprovisioned(http.StatusForbidden, body) {
		t.Error("403 + body should match")
	}
	if hubHeartbeatRejectsUnprovisioned(http.StatusUnauthorized, body) {
		t.Error("401 must not match")
	}
}

func TestUnprovisionedWarnIsLogFedClass(t *testing.T) {
	msg := "hub heartbeat rejected: hive id is not provisioned on the hub — backing off"
	if got := fleetreport.LogFedClass(msg); got != fleetreport.ClassHubHeartbeatUnprovisioned {
		t.Fatalf("LogFedClass = %q", got)
	}
}

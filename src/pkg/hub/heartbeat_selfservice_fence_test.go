package hub

import (
	"reflect"
	"sort"
	"testing"
)

// TestHeartbeatResponseFieldFenceInventory fails when a HeartbeatResponse
// field has no credential-fence classification, or when the fence names a
// field that no longer exists. A new field must be classified as allowed or
// fenced before it can ship (#18060).
func TestHeartbeatResponseFieldFenceInventory(t *testing.T) {
	typ := reflect.TypeOf(HeartbeatResponse{})
	seen := map[string]bool{}
	var missing []string
	for i := 0; i < typ.NumField(); i++ {
		name := typ.Field(i).Name
		seen[name] = true
		switch heartbeatResponseFieldFence[name] {
		case heartbeatFieldAllowed, heartbeatFieldFenced:
		default:
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		t.Errorf("HeartbeatResponse fields with no credential-fence classification: %v\n"+
			"add each to heartbeatResponseFieldFence in heartbeat_selfservice_fence.go as "+
			"heartbeatFieldAllowed (protocol/display only) or heartbeatFieldFenced "+
			"(credential, management action, hub policy, or spoke-reported identity)", missing)
	}
	var stale []string
	for name := range heartbeatResponseFieldFence {
		if !seen[name] {
			stale = append(stale, name)
		}
	}
	if len(stale) > 0 {
		sort.Strings(stale)
		t.Errorf("heartbeatResponseFieldFence names fields HeartbeatResponse no longer has: %v", stale)
	}
}

// The fields the RFC names explicitly must stay fenced, so a later edit to the
// table cannot quietly reclassify a credential as allowed.
func TestHeartbeatResponseFieldFenceCoversCredentialsAndManagement(t *testing.T) {
	for _, name := range []string{
		"GitHubAppConfig", "PendingGateway", "ProjectConfig", "ClusterID",
		"UpgradeTo", "UpgradeInterrupt", "SwitchToTag", "UpgradeAnnounce",
		"RestartSpoke", "AuthorizedUsers", "AuthorizedUserNames",
	} {
		if heartbeatResponseFieldFence[name] != heartbeatFieldFenced {
			t.Errorf("%s must be heartbeatFieldFenced", name)
		}
	}
}

func TestIsSelfServiceHiveID(t *testing.T) {
	cases := map[string]bool{
		"byo-alice-lab":     true,
		"BYO-alice-lab":     true,
		" byo-x":            true,
		"byo":               false,
		"hosted-acme":       false,
		"saas-acme":         false,
		"kelly-headwaters":  false,
		"my-byo-hive":       false,
		"":                  false,
		"byoverlay-cluster": false,
	}
	for id, want := range cases {
		if got := IsSelfServiceHiveID(id); got != want {
			t.Errorf("IsSelfServiceHiveID(%q) = %v, want %v", id, got, want)
		}
	}
}

// filledHeartbeatResponse sets every exported field of a HeartbeatResponse to
// a non-zero value, so the fence test covers fields added after it was written.
func filledHeartbeatResponse(t *testing.T) *HeartbeatResponse {
	t.Helper()
	resp := &HeartbeatResponse{}
	v := reflect.ValueOf(resp).Elem()
	for i := 0; i < v.NumField(); i++ {
		fv := v.Field(i)
		if !fv.CanSet() {
			continue
		}
		fillNonZero(t, v.Type().Field(i).Name, fv)
		if fv.IsZero() {
			t.Fatalf("could not give HeartbeatResponse.%s a non-zero value", v.Type().Field(i).Name)
		}
	}
	return resp
}

func fillNonZero(t *testing.T, name string, fv reflect.Value) {
	t.Helper()
	switch fv.Kind() {
	case reflect.Bool:
		fv.SetBool(true)
	case reflect.String:
		fv.SetString("x")
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		fv.SetInt(1)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		fv.SetUint(1)
	case reflect.Slice:
		fv.Set(reflect.MakeSlice(fv.Type(), 1, 1))
	case reflect.Map:
		fv.Set(reflect.MakeMap(fv.Type()))
		fv.SetMapIndex(reflect.Zero(fv.Type().Key()), reflect.Zero(fv.Type().Elem()))
	case reflect.Pointer:
		fv.Set(reflect.New(fv.Type().Elem()))
	case reflect.Struct:
		for i := 0; i < fv.NumField(); i++ {
			if fv.Field(i).CanSet() {
				fillNonZero(t, name, fv.Field(i))
				return
			}
		}
	default:
		t.Fatalf("fillNonZero: unhandled kind %s for HeartbeatResponse.%s", fv.Kind(), name)
	}
}

func TestFenceSelfServiceHeartbeatResponseZeroesFencedFields(t *testing.T) {
	resp := filledHeartbeatResponse(t)
	withheld := fenceSelfServiceHeartbeatResponse(resp, "byo-alice-lab")

	withheldSet := map[string]bool{}
	for _, name := range withheld {
		withheldSet[name] = true
	}
	v := reflect.ValueOf(resp).Elem()
	for i := 0; i < v.NumField(); i++ {
		name := v.Type().Field(i).Name
		switch heartbeatResponseFieldFence[name] {
		case heartbeatFieldAllowed:
			if v.Field(i).IsZero() {
				t.Errorf("allowed field %s was cleared", name)
			}
			if withheldSet[name] {
				t.Errorf("allowed field %s reported as withheld", name)
			}
		default:
			if !v.Field(i).IsZero() {
				t.Errorf("fenced field %s still set for a self-service hive", name)
			}
			if !withheldSet[name] {
				t.Errorf("fenced field %s cleared but not reported as withheld", name)
			}
		}
	}
}

func TestFenceSelfServiceHeartbeatResponseLeavesOtherHivesAlone(t *testing.T) {
	for _, id := range []string{"hosted-acme", "kelly-headwaters", ""} {
		resp := filledHeartbeatResponse(t)
		want := *resp
		if withheld := fenceSelfServiceHeartbeatResponse(resp, id); withheld != nil {
			t.Errorf("hive %q: withheld %v, want nothing", id, withheld)
		}
		if !reflect.DeepEqual(*resp, want) {
			t.Errorf("hive %q: response changed by the fence", id)
		}
	}
	if fenceSelfServiceHeartbeatResponse(nil, "byo-x") != nil {
		t.Error("nil response must be a no-op")
	}
}

// End to end through the heartbeat handler: hub-held credentials queued for
// a self-service hive ID never reach the wire, while the same queue for an
// operator-listed hive still delivers, and display-only data still flows.
func TestHeartbeatHandlerFencesSelfServiceHive(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()

	queue := func(s *HubServer, id string) {
		s.pendingGitHubAppConfigs[id] = &HeartbeatGitHubAppConfig{AppID: 42, InstallationID: 7, PrivateKey: "-----BEGIN KEY-----"}
		s.pendingGateways[id] = &HeartbeatGatewayConfig{Name: "openrouter", Kind: "openrouter", Endpoint: "https://example.invalid", Key: "sk-secret"}
		s.hubBanners[id] = &HubBannerEntry{ID: "b1", Message: "maintenance tonight", Color: "info"}
	}

	t.Run("self-service hive gets no credentials", func(t *testing.T) {
		s := newHeartbeatHub()
		const id = "byo-alice-lab"
		queue(s, id)
		resp := decodeHeartbeatResponse(t, s, `{"hive_id":"`+id+`","org":"alice","cluster_id":"hive-oke"}`)
		if resp.GitHubAppConfig != nil {
			t.Errorf("GitHubAppConfig delivered to a self-service hive: %+v", resp.GitHubAppConfig)
		}
		if resp.PendingGateway != nil {
			t.Errorf("PendingGateway delivered to a self-service hive: %+v", resp.PendingGateway)
		}
		if resp.UpgradePolicy != nil {
			t.Errorf("UpgradePolicy delivered to a self-service hive: %+v", resp.UpgradePolicy)
		}
		if !resp.OK || resp.HubGitHash == "" {
			t.Errorf("protocol fields lost: ok=%v hub_git_hash=%q", resp.OK, resp.HubGitHash)
		}
		if resp.HubBanner == nil || resp.HubBanner.Message != "maintenance tonight" {
			t.Errorf("display-only banner not delivered: %+v", resp.HubBanner)
		}
	})

	t.Run("operator-listed hive still gets them", func(t *testing.T) {
		s := newHeartbeatHub()
		const id = "alice-lab"
		queue(s, id)
		resp := decodeHeartbeatResponse(t, s, `{"hive_id":"`+id+`","org":"alice","cluster_id":"hive-oke"}`)
		if resp.GitHubAppConfig == nil || resp.GitHubAppConfig.PrivateKey == "" {
			t.Errorf("GitHubAppConfig not delivered to an operator-listed hive: %+v", resp.GitHubAppConfig)
		}
		if resp.PendingGateway == nil {
			t.Error("PendingGateway not delivered to an operator-listed hive")
		}
	})
}

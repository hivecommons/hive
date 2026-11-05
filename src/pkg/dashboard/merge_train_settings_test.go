package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
)

func patchMergeTrainSettings(s *Server, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPatch, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	markOwnerRequest(req)
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	return rec
}

func TestMergeTrainSettingsPatchRoundTrip(t *testing.T) {
	s := covApiServer(t)
	assertTrain := func(enabled bool, attempts int) {
		t.Helper()
		rec := doOwnerGet(s, "/api/config/auto-merge")
		var body struct {
			Train struct {
				Enabled  bool `json:"enabled"`
				Attempts int  `json:"max_rebase_attempts"`
			} `json:"merge_train"`
			Bots []map[string]any `json:"bot_authors"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if rec.Code != http.StatusOK || body.Train.Enabled != enabled || body.Train.Attempts != attempts || len(body.Bots) == 0 {
			t.Fatalf("auto-merge response: %d %s", rec.Code, rec.Body.String())
		}
	}
	assertTrain(true, 2)
	for _, body := range []string{`{"merge_train":{"enabled":false,"max_rebase_attempts":4}}`, `{"merge_train":{"max_rebase_attempts":0}}`, `{"merge_train":{"enabled":true}}`} {
		if rec := patchMergeTrainSettings(s, "/api/config/auto-merge", body); rec.Code != http.StatusOK {
			t.Fatalf("PATCH: %d %s", rec.Code, rec.Body.String())
		}
		switch body {
		case `{"merge_train":{"enabled":false,"max_rebase_attempts":4}}`:
			assertTrain(false, 4)
		case `{"merge_train":{"max_rebase_attempts":0}}`:
			assertTrain(false, 2)
		default:
			assertTrain(true, 2)
		}
	}
	// The existing partial PUT writer accepts the same nested payload.
	if rec := doPut(s, "/api/config/auto-merge", map[string]any{"merge_train": map[string]any{"max_rebase_attempts": 3}}); rec.Code != http.StatusOK {
		t.Fatal(rec.Body.String())
	}
	assertTrain(true, 3)
	for _, seconds := range []int{7200, 0, 3600} {
		raw, _ := json.Marshal(map[string]int{"merge_train_stall_warn_s": seconds})
		rec := patchMergeTrainSettings(s, "/api/config/governor", string(raw))
		if rec.Code != http.StatusOK {
			t.Fatalf("governor PATCH: %d %s", rec.Code, rec.Body.String())
		}
		var body map[string]int
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if got := body["merge_train_stall_warn_s"]; got != seconds {
			t.Fatalf("echo=%d want %d", got, seconds)
		}
		rec = doOwnerGet(s, "/api/config/governor")
		var governor map[string]json.RawMessage
		if err := json.Unmarshal(rec.Body.Bytes(), &governor); err != nil {
			t.Fatal(err)
		}
		if string(governor["merge_train_stall_warn_s"]) != strconv.Itoa(seconds) {
			t.Fatalf("governor GET: %s", rec.Body.String())
		}
		if s.deps.Config.MergeTrainStallWarn() != time.Duration(seconds)*time.Second {
			t.Fatal("stall threshold not applied")
		}
	}
	for _, action := range []string{"config_auto_merge", "config_governor"} {
		found := false
		for _, entry := range s.audit.Recent(50) {
			if entry.Action == action {
				if !auditEntryIsSensitive(entry) {
					t.Errorf("%s audit must be marked sensitive", action)
				}
				found = true
			}
		}
		if !found {
			t.Errorf("missing %s audit", action)
		}
	}
}

func TestMergeTrainSettingsPatchRejectsInvalidWithoutMutation(t *testing.T) {
	for _, tc := range []struct{ path, body string }{
		{"auto-merge", `{"merge_train":{"enabled":false,"max_rebase_attempts":-1}}`},
		{"auto-merge", `{"merge_train":{"max_rebase_attempts":"two"}}`},
		{"auto-merge", `{"merge_train":"invalid"}`},
		{"governor", `{"merge_train_stall_warn_s":"soon"}`},
		{"governor", `{"merge_train_stall_warn_s":-1}`},
		{"governor", `{"merge_train_stall_warn_s":1.5}`},
		{"governor", `{"merge_train_stall_warn_s":9223372036854775807}`},
		{"governor", `"invalid"`},
	} {
		t.Run(tc.path+tc.body, func(t *testing.T) {
			s := covApiServer(t)
			beforeAM, beforeGov := s.deps.Config.AutoMerge, s.deps.Config.Governor
			rec := patchMergeTrainSettings(s, "/api/config/"+tc.path, tc.body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("got %d: %s", rec.Code, rec.Body.String())
			}
			if !reflect.DeepEqual(beforeAM, s.deps.Config.AutoMerge) || !reflect.DeepEqual(beforeGov, s.deps.Config.Governor) {
				t.Fatal("invalid PATCH mutated settings")
			}
		})
	}
}

func TestMergeTrainSettingsOwnerOnlyAndPersistenceFailure(t *testing.T) {
	for _, tc := range []struct{ path, body string }{
		{"auto-merge", `{"merge_train":{"enabled":false,"max_rebase_attempts":3}}`},
		{"governor", `{"merge_train_stall_warn_s":0}`},
	} {
		t.Run(tc.path, func(t *testing.T) {
			s := covApiServer(t)
			beforeAM, beforeGov := s.deps.Config.AutoMerge, s.deps.Config.Governor
			req := httptest.NewRequest(http.MethodPatch, "/api/config/"+tc.path, strings.NewReader(tc.body))
			req.Header.Set("X-Hive-Role", config.RoleReadWrite)
			rec := httptest.NewRecorder()
			s.mux.ServeHTTP(rec, req)
			if rec.Code != http.StatusForbidden {
				t.Fatalf("non-owner: %d", rec.Code)
			}
			s.deps.Config.SourcePath = "unwritten-merge-train-settings.yaml"
			s.deps.Config.Project.Org = "" // Save guard fails before writing files.
			rec = patchMergeTrainSettings(s, "/api/config/"+tc.path, tc.body)
			if rec.Code != http.StatusInternalServerError {
				t.Fatalf("save failure: %d %s", rec.Code, rec.Body.String())
			}
			if !reflect.DeepEqual(beforeAM, s.deps.Config.AutoMerge) || !reflect.DeepEqual(beforeGov, s.deps.Config.Governor) {
				t.Fatal("refused update changed live settings")
			}
		})
	}
}

func TestMergeTrainSettingsMarkup(t *testing.T) {
	body := extractJSFunction(t, indexHTML(t), "renderGovFeatures")
	for _, key := range []string{"auto_merge.merge_train.enabled", "auto_merge.merge_train.max_rebase_attempts", "governor.merge_train_stall_warn_s"} {
		if !strings.Contains(body, "yamlHint('"+key+"')") {
			t.Errorf("missing yamlHint for %s", key)
		}
	}
	for _, label := range []string{"Conflict-aware merge train", "Rebase attempts per front", "Stall warning after"} {
		if !strings.Contains(body, "'"+label+"', `") {
			t.Errorf("missing settingHelpMark for %s", label)
		}
	}
	train, native, review := strings.Index(body, ">Merge train</div>"), strings.Index(body, "<label>Native GitHub merge queue"), strings.Index(body, "featuresSectionHtml('review'")
	if train < 0 || native <= train || review <= native {
		t.Fatal("merge train and native queue must appear together in Auto-merge above Review Gate")
	}
	if !strings.Contains(body, "merge_train_stall_warn_s ?? 3600") {
		t.Fatal("stall input must preserve an explicit zero")
	}
}

package github

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/priorart"
)

// newPriorArtIssueMockServer is newIssueMockServer that also records the
// body of each POST /issues.
func newPriorArtIssueMockServer(t *testing.T, createdBodies *[]string) *httptest.Server {
	t.Helper()
	inner := newIssueMockServer(t, "", nil, nil, nil)
	t.Cleanup(inner.Close)
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/issues") {
			raw, _ := io.ReadAll(r.Body)
			var payload struct {
				Body string `json:"body"`
			}
			_ = json.Unmarshal(raw, &payload)
			*createdBodies = append(*createdBodies, payload.Body)
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"id":990099,"number":99,"html_url":"https://github.example/o/r/issues/99"}`)
			return
		}
		inner.Config.Handler.ServeHTTP(w, r)
	}))
}

func priorArtTestReport(confidence int) priorart.Report {
	return priorart.Report{
		Confidence: confidence,
		Findings: []priorart.Finding{{
			Candidate: priorart.Candidate{
				Kind:  priorart.KindMergedPR,
				Title: "Add retry backoff to the relay",
				URL:   "https://github.example/o/r/pull/42",
				Ref:   "#42",
			},
			Confidence: confidence,
			Matched:    []string{"backoff", "relay"},
		}},
	}
}

type priorArtBead struct{ title, detail, agent, ref string }

// The relay enforces priorart.FilingVerdictFor on hive-agent issue requests
// (hivecommons/hive#13879): DoNotFile refuses naming the match and records
// a bead, FileWithFindings requires a "Gap vs existing" section and
// pre-attaches the findings note, FileAsIs files unchanged, and a disabled
// gate or a non-feature kind is never searched.
func TestIssueRequestWatcher_PriorArtFilingVerdict(t *testing.T) {
	off := false
	gapBody := "Relays should back off.\n\n## Gap vs existing\n\n#42 only covers PR requests, not issue requests."
	cases := []struct {
		name         string
		gate         config.PriorArtGateConfig
		title        string
		body         string
		confidence   int
		wantCreated  bool
		wantSearched bool
		wantBead     bool
		wantErr      []string
		wantBody     []string
		wantNoBody   []string
		// wantAudit is the verdict pair an agent_issue_prior_art_refused
		// audit record must carry; empty means no such record.
		wantAudit string
	}{
		{
			name:  "do not file: high confidence refuses and records a bead",
			title: "[feature] relay retry backoff", body: gapBody, confidence: 90,
			wantSearched: true, wantBead: true, wantAudit: "do_not_file",
			wantErr: []string{"not filed", "90%", "#42", "merged PR", "Add retry backoff to the relay", "https://github.example/o/r/pull/42", "knowledge bead"},
		},
		{
			name:  "file with findings: gap section present files with the findings note",
			title: "[feature] relay retry backoff", body: gapBody, confidence: 60,
			wantCreated: true, wantSearched: true,
			wantBody: []string{"## Gap vs existing", priorart.FindingsMarker, "https://github.example/o/r/pull/42"},
		},
		{
			name:  "file with findings: missing gap section is refused with what to add",
			title: "[feature] relay retry backoff", body: "Relays should back off.", confidence: 60,
			wantSearched: true, wantAudit: "missing_gap_section",
			wantErr: []string{"not filed", "add a \"## Gap vs existing\" section", "## Gap vs existing", "https://github.example/o/r/pull/42"},
		},
		{
			name:  "file as is: low confidence files unchanged",
			title: "[feature] relay retry backoff", body: "Relays should back off.", confidence: 20,
			wantCreated: true, wantSearched: true,
			wantBody:   []string{"Relays should back off."},
			wantNoBody: []string{priorart.FindingsMarker},
		},
		{
			name:  "gate disabled: never searched, files unchanged",
			gate:  config.PriorArtGateConfig{Enabled: &off},
			title: "[feature] relay retry backoff", body: "Relays should back off.", confidence: 90,
			wantCreated: true,
			wantNoBody:  []string{priorart.FindingsMarker},
		},
		{
			name:  "non-feature kind: never searched, files unchanged",
			title: "[bug] relay drops requests", body: "It drops them.", confidence: 90,
			wantCreated: true,
			wantNoBody:  []string{priorart.FindingsMarker},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var bodies []string
			srv := newPriorArtIssueMockServer(t, &bodies)
			defer srv.Close()
			c := issueTestClient(t, srv.URL)
			dir := withIssueDir(t)

			searched := false
			var beads []priorArtBead
			var audits []AuditRecord
			c.SetAttributionAuditRecord(func(rec AuditRecord) {
				if rec.Action == AuditActionAgentIssuePriorArtRefused {
					audits = append(audits, rec)
				}
			})
			c.SetPriorArtFilingGate(
				func() config.PriorArtGateConfig { return tc.gate },
				func(ctx context.Context, issue priorart.Issue) priorart.Report {
					searched = true
					return priorArtTestReport(tc.confidence)
				},
				func(title, detail, agent, ref string) {
					beads = append(beads, priorArtBead{title, detail, agent, ref})
				},
			)

			reqPath, err := WriteIssueRequest(dir, IssueRequest{
				Repo: "o/r", Title: tc.title, Body: tc.body,
				Labels: []string{"agent/scanner"}, Agent: "scanner",
			})
			if err != nil {
				t.Fatal(err)
			}
			c.ProcessIssueRequestsOnce(context.Background())

			if searched != tc.wantSearched {
				t.Errorf("searched = %v, want %v", searched, tc.wantSearched)
			}
			if got := len(bodies) == 1; got != tc.wantCreated {
				t.Fatalf("created = %v (%d creates), want %v", got, len(bodies), tc.wantCreated)
			}
			if _, err := os.Stat(reqPath); !os.IsNotExist(err) {
				t.Errorf("request file should be consumed")
			}
			var res IssueResponse
			b, err := os.ReadFile(strings.TrimSuffix(reqPath, ".json") + ".result.json")
			if err != nil {
				t.Fatalf("result file missing: %v", err)
			}
			if err := json.Unmarshal(b, &res); err != nil {
				t.Fatal(err)
			}
			if res.OK != tc.wantCreated {
				t.Errorf("result OK = %v, want %v (%+v)", res.OK, tc.wantCreated, res)
			}
			for _, want := range tc.wantErr {
				if !strings.Contains(res.Error, want) {
					t.Errorf("result error %q missing %q", res.Error, want)
				}
			}
			if tc.wantCreated {
				for _, want := range tc.wantBody {
					if !strings.Contains(bodies[0], want) {
						t.Errorf("created body missing %q:\n%s", want, bodies[0])
					}
				}
				for _, unwanted := range tc.wantNoBody {
					if strings.Contains(bodies[0], unwanted) {
						t.Errorf("created body unexpectedly contains %q:\n%s", unwanted, bodies[0])
					}
				}
			}
			if got := len(beads) == 1; got != tc.wantBead {
				t.Fatalf("bead recorded = %v (%d), want %v", got, len(beads), tc.wantBead)
			}
			if tc.wantBead {
				bead := beads[0]
				if bead.agent != "scanner" || bead.ref != "https://github.example/o/r/pull/42" ||
					!strings.Contains(bead.title, "relay retry backoff") || !strings.Contains(bead.detail, "#42") ||
					!strings.Contains(bead.detail, "merged PR") {
					t.Errorf("unexpected bead: %+v", bead)
				}
			}
			if got := len(audits) == 1; got != (tc.wantAudit != "") {
				t.Fatalf("prior-art refusal audits = %d, want one = %v", len(audits), tc.wantAudit != "")
			}
			if tc.wantAudit != "" {
				rec := audits[0]
				for _, want := range []string{"verdict=" + tc.wantAudit, "match=https://github.example/o/r/pull/42", "kind=merged_pr", "confidence=" + strconv.Itoa(tc.confidence)} {
					if !strings.Contains(rec.Detail, want) {
						t.Errorf("audit detail %q missing %q", rec.Detail, want)
					}
				}
				if rec.Agent != "scanner" || rec.Repo != "o/r" {
					t.Errorf("audit agent/repo = %q/%q, want scanner/o/r", rec.Agent, rec.Repo)
				}
			}
		})
	}
}

// Without the gate wired the relay files exactly as before.
func TestIssueRequestWatcher_PriorArtFilingGateUnwired(t *testing.T) {
	var bodies []string
	srv := newPriorArtIssueMockServer(t, &bodies)
	defer srv.Close()
	c := issueTestClient(t, srv.URL)
	dir := withIssueDir(t)
	if _, err := WriteIssueRequest(dir, IssueRequest{
		Repo: "o/r", Title: "[feature] relay retry backoff", Body: "Relays should back off.",
		Labels: []string{"agent/scanner"}, Agent: "scanner",
	}); err != nil {
		t.Fatal(err)
	}
	c.ProcessIssueRequestsOnce(context.Background())
	if len(bodies) != 1 || strings.Contains(bodies[0], priorart.FindingsMarker) {
		t.Fatalf("unwired gate must file unchanged, got %d creates: %v", len(bodies), bodies)
	}
}

func TestHasGapVsExistingSection(t *testing.T) {
	cases := []struct {
		body string
		want bool
	}{
		{"## Gap vs existing\n\nx", true},
		{"### gap vs existing", true},
		{"**Gap vs existing** — the PR misses issues", true},
		{"Gap vs. existing:", true},
		{"No gap stated here.", false},
		{"", false},
	}
	for _, tc := range cases {
		if got := hasGapVsExistingSection(tc.body); got != tc.want {
			t.Errorf("hasGapVsExistingSection(%q) = %v, want %v", tc.body, got, tc.want)
		}
	}
}

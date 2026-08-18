package knowledge

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	gh "github.com/google/go-github/v72/github"
)

func maturityTestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

func ghClientFromServer(server *httptest.Server) *gh.Client {
	client := gh.NewClient(nil)
	client.BaseURL, _ = client.BaseURL.Parse(server.URL + "/")
	return client
}

func TestNewMaturityDetector(t *testing.T) {
	ghClient := gh.NewClient(nil)
	d := NewMaturityDetector(ghClient, maturityTestLogger())
	if d == nil {
		t.Fatal("NewMaturityDetector returned nil")
	}
	if d.ghClient != ghClient {
		t.Error("ghClient not set")
	}
}

func TestDetect_Idea(t *testing.T) {
	// Server returns zero results for everything
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/search/code":
			json.NewEncoder(w).Encode(map[string]interface{}{
				"total_count": 0,
				"items":       []interface{}{},
			})
		case r.URL.Path == "/repos/owner/repo/actions/workflows":
			json.NewEncoder(w).Encode(map[string]interface{}{
				"total_count": 0,
				"workflows":   []interface{}{},
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	client := ghClientFromServer(server)
	d := NewMaturityDetector(client, maturityTestLogger())
	result, err := d.Detect(context.Background(), "owner", "repo")
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if result.Level != MaturityIdea {
		t.Errorf("level = %d (%s), want %d (idea)", result.Level, result.Level.String(), MaturityIdea)
	}
	if result.TestMode != "suggest" {
		t.Errorf("test mode = %q, want suggest", result.TestMode)
	}
}

func TestDetect_Dev(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/search/code", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]interface{}{
			"total_count": 5,
			"items":       []interface{}{},
		})
	})
	mux.HandleFunc("/repos/owner/repo/actions/workflows", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]interface{}{
			"total_count": 0,
			"workflows":   []interface{}{},
		})
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	client := ghClientFromServer(server)
	d := NewMaturityDetector(client, maturityTestLogger())
	result, err := d.Detect(context.Background(), "owner", "repo")
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if result.Level != MaturityDev {
		t.Errorf("level = %s, want development", result.Level.String())
	}
}

func TestDetect_CI(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/search/code", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]interface{}{
			"total_count": 10,
			"items":       []interface{}{},
		})
	})
	mux.HandleFunc("/repos/owner/repo/actions/workflows", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]interface{}{
			"total_count": 2,
			"workflows":   []interface{}{},
		})
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	client := ghClientFromServer(server)
	d := NewMaturityDetector(client, maturityTestLogger())
	result, err := d.Detect(context.Background(), "owner", "repo")
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if result.Level != MaturityCI {
		t.Errorf("level = %s, want ci-cd", result.Level.String())
	}
	if result.TestMode != "gate" {
		t.Errorf("test mode = %q, want gate", result.TestMode)
	}
}

func TestDetect_FullAuto(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/search/code", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]interface{}{
			"total_count": 50,
			"items":       []interface{}{},
		})
	})
	mux.HandleFunc("/repos/owner/repo/actions/workflows", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]interface{}{
			"total_count": 3,
			"workflows":   []interface{}{},
		})
	})
	// Coverage config found
	mux.HandleFunc("/repos/owner/repo/contents/.codecov.yml", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]interface{}{
			"name":    ".codecov.yml",
			"content": "Y292ZXJhZ2U6",
			"type":    "file",
		})
	})
	// TDD markers found
	mux.HandleFunc("/repos/owner/repo/contents/CONTRIBUTING.md", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]interface{}{
			"name":     "CONTRIBUTING.md",
			"content":  "V2UgdXNlIHRkZCBhcHByb2FjaA==", // "We use tdd approach"
			"encoding": "base64",
			"type":     "file",
		})
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	client := ghClientFromServer(server)
	d := NewMaturityDetector(client, maturityTestLogger())
	result, err := d.Detect(context.Background(), "owner", "repo")
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if result.Level != MaturityFullAuto {
		t.Errorf("level = %s, want full-auto", result.Level.String())
	}
	if result.TestMode != "tdd" {
		t.Errorf("test mode = %q, want tdd", result.TestMode)
	}
}

func TestDetectCoverageGap_NoRuns(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/owner/repo/actions/workflows/ci.yml/runs", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]interface{}{
			"total_count":   0,
			"workflow_runs": []interface{}{},
		})
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	client := ghClientFromServer(server)
	d := NewMaturityDetector(client, maturityTestLogger())
	gap, err := d.DetectCoverageGap(context.Background(), "owner", "repo")
	if err != nil {
		t.Fatalf("DetectCoverageGap: %v", err)
	}
	if gap.TargetPct != defaultCoverageTarget {
		t.Errorf("target = %f, want %f", gap.TargetPct, defaultCoverageTarget)
	}
	if gap.GapPct != defaultCoverageTarget {
		t.Errorf("gap = %f", gap.GapPct)
	}
}

func TestDetectCoverageGap_WithRuns(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/owner/repo/actions/workflows/ci.yml/runs", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]interface{}{
			"total_count": 1,
			"workflow_runs": []map[string]interface{}{
				{"id": 123, "status": "completed"},
			},
		})
	})
	mux.HandleFunc("/repos/owner/repo/actions/runs/123/artifacts", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]interface{}{
			"total_count": 1,
			"artifacts": []map[string]interface{}{
				{"name": "coverage-report", "id": 1},
			},
		})
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	client := ghClientFromServer(server)
	d := NewMaturityDetector(client, maturityTestLogger())
	gap, err := d.DetectCoverageGap(context.Background(), "owner", "repo")
	if err != nil {
		t.Fatalf("DetectCoverageGap: %v", err)
	}
	if gap.TargetPct != defaultCoverageTarget {
		t.Errorf("target = %f", gap.TargetPct)
	}
}

func TestClassifyMaturity_AllCases(t *testing.T) {
	tests := []struct {
		name    string
		signals MaturitySignals
		want    MaturityLevel
	}{
		{"no tests no ci", MaturitySignals{}, MaturityIdea},
		{"tests only", MaturitySignals{HasTests: true}, MaturityDev},
		{"ci only", MaturitySignals{HasCI: true}, MaturityCI},
		{"ci and coverage", MaturitySignals{HasCI: true, HasCoverageConfig: true}, MaturityCI},
		{"full auto", MaturitySignals{HasTests: true, HasCI: true, HasCoverageConfig: true, HasTDDMarkers: true}, MaturityFullAuto},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := classifyMaturity(tt.signals)
			if got != tt.want {
				t.Errorf("classifyMaturity = %s, want %s", got.String(), tt.want.String())
			}
		})
	}
}

func TestMaturityLevel_String_Unknown(t *testing.T) {
	level := MaturityLevel(99)
	s := level.String()
	if s != "unknown(99)" {
		t.Errorf("String = %q", s)
	}
}

func TestMaturityLevel_TestMode_Unknown(t *testing.T) {
	level := MaturityLevel(99)
	mode := level.TestMode()
	if mode != "suggest" {
		t.Errorf("TestMode = %q, want suggest", mode)
	}
}

func TestDetect_TestFileSearchError(t *testing.T) {
	// Return 500 for search/code to trigger countTestFiles error path
	mux := http.NewServeMux()
	mux.HandleFunc("/search/code", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	mux.HandleFunc("/repos/owner/repo/actions/workflows", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]interface{}{
			"total_count": 0,
			"workflows":   []interface{}{},
		})
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	client := ghClientFromServer(server)
	d := NewMaturityDetector(client, maturityTestLogger())
	result, err := d.Detect(context.Background(), "owner", "repo")
	if err != nil {
		t.Fatalf("Detect should not fail: %v", err)
	}
	// With error, testCount is 0, so HasTests = false
	if result.Signals.HasTests {
		t.Error("HasTests should be false when search API fails")
	}
}

func TestDetect_CIWorkflowsError(t *testing.T) {
	// Search returns tests but workflows endpoint fails
	mux := http.NewServeMux()
	mux.HandleFunc("/search/code", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]interface{}{
			"total_count": 5,
			"items":       []interface{}{},
		})
	})
	mux.HandleFunc("/repos/owner/repo/actions/workflows", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	client := ghClientFromServer(server)
	d := NewMaturityDetector(client, maturityTestLogger())
	result, err := d.Detect(context.Background(), "owner", "repo")
	if err != nil {
		t.Fatalf("Detect should not fail: %v", err)
	}
	if !result.Signals.HasTests {
		t.Error("HasTests should be true")
	}
	if result.Signals.HasCI {
		t.Error("HasCI should be false when workflows API fails")
	}
}

func TestDetectCoverageGap_ArtifactsError(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/owner/repo/actions/workflows/ci.yml/runs", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]interface{}{
			"total_count": 1,
			"workflow_runs": []map[string]interface{}{
				{"id": 123, "status": "completed"},
			},
		})
	})
	mux.HandleFunc("/repos/owner/repo/actions/runs/123/artifacts", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	client := ghClientFromServer(server)
	d := NewMaturityDetector(client, maturityTestLogger())
	gap, err := d.DetectCoverageGap(context.Background(), "owner", "repo")
	if err != nil {
		t.Fatalf("DetectCoverageGap: %v", err)
	}
	if gap.TargetPct != defaultCoverageTarget {
		t.Errorf("target = %f, want %f", gap.TargetPct, defaultCoverageTarget)
	}
}

func TestDetectCoverageGap_NoCoverageArtifact(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/owner/repo/actions/workflows/ci.yml/runs", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]interface{}{
			"total_count": 1,
			"workflow_runs": []map[string]interface{}{
				{"id": 123, "status": "completed"},
			},
		})
	})
	mux.HandleFunc("/repos/owner/repo/actions/runs/123/artifacts", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]interface{}{
			"total_count": 1,
			"artifacts": []map[string]interface{}{
				{"name": "build-output", "id": 1},
			},
		})
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	client := ghClientFromServer(server)
	d := NewMaturityDetector(client, maturityTestLogger())
	gap, err := d.DetectCoverageGap(context.Background(), "owner", "repo")
	if err != nil {
		t.Fatalf("DetectCoverageGap: %v", err)
	}
	// No coverage artifact found — should return default gap
	if gap.GapPct != defaultCoverageTarget {
		t.Errorf("gap = %f, want %f", gap.GapPct, defaultCoverageTarget)
	}
}

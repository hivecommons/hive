package scheduler

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/knowledge"
)

// knowledgeLogLines returns the decoded "knowledge: injected …" records a
// scheduler wrote to buf.
func knowledgeLogLines(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("bad log line %q: %v", line, err)
		}
		if msg, _ := rec["msg"].(string); strings.HasPrefix(msg, "knowledge: injected ") {
			out = append(out, rec)
		}
	}
	return out
}

func TestLogKnowledgeInjection(t *testing.T) {
	tests := []struct {
		name          string
		inj           knowledgeInjection
		agentsMdBytes int
		skillsBytes   int
		wantLine      bool
		wantMsg       string
	}{
		{
			name:     "primer did not run and nothing else injected: silent",
			inj:      knowledgeInjection{},
			wantLine: false,
		},
		{
			name:     "primed with nothing found still reports a zero line",
			inj:      knowledgeInjection{Primed: true, CapBytes: 4096},
			wantLine: true,
			wantMsg:  "knowledge: injected 0 bytes from 0 entries (skipped 0 for cap)",
		},
		{
			name:          "primed with omissions plus AGENTS.md and skills",
			inj:           knowledgeInjection{Primed: true, Bytes: 3900, Entries: 7, SkippedForCap: 3, CapBytes: 4096},
			agentsMdBytes: 1980,
			skillsBytes:   512,
			wantLine:      true,
			wantMsg:       "knowledge: injected 3900 bytes from 7 entries (skipped 3 for cap)",
		},
		{
			name:          "no primer but an AGENTS.md block is still a knowledge payload",
			inj:           knowledgeInjection{},
			agentsMdBytes: 1980,
			wantLine:      true,
			wantMsg:       "knowledge: injected 0 bytes from 0 entries (skipped 0 for cap)",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			s := New(&config.Config{}, slog.New(slog.NewJSONHandler(&buf, nil)))
			s.logKnowledgeInjection("developer", tc.inj, tc.agentsMdBytes, tc.skillsBytes)
			lines := knowledgeLogLines(t, &buf)
			if !tc.wantLine {
				if len(lines) != 0 {
					t.Fatalf("expected no line, got %v", lines)
				}
				return
			}
			if len(lines) != 1 {
				t.Fatalf("expected exactly one line, got %d: %v", len(lines), lines)
			}
			rec := lines[0]
			if rec["msg"] != tc.wantMsg {
				t.Fatalf("msg = %q, want %q", rec["msg"], tc.wantMsg)
			}
			want := map[string]float64{
				"injected_bytes":  float64(tc.inj.Bytes),
				"entries":         float64(tc.inj.Entries),
				"skipped_for_cap": float64(tc.inj.SkippedForCap),
				"cap_bytes":       float64(tc.inj.CapBytes),
				"agents_md_bytes": float64(tc.agentsMdBytes),
				"skills_bytes":    float64(tc.skillsBytes),
			}
			for k, v := range want {
				if got, _ := rec[k].(float64); got != v {
					t.Errorf("%s = %v, want %v", k, rec[k], v)
				}
			}
			if rec["agent"] != "developer" {
				t.Errorf("agent = %v, want developer", rec["agent"])
			}
			if rec["primed"] != tc.inj.Primed {
				t.Errorf("primed = %v, want %v", rec["primed"], tc.inj.Primed)
			}
		})
	}
}

// TestPrimeKnowledge_LogsOneLinePerKick drives the real primer against a temp
// vault with a cap small enough to drop an entry, and checks that
// primeKnowledge emits exactly one summary line whose numbers match the
// rendered section.
func TestPrimeKnowledge_LogsOneLinePerKick(t *testing.T) {
	dir := t.TempDir()
	long := strings.Repeat("scheduler kick detail. ", 40)
	for i, name := range []string{"scheduler-a.md", "scheduler-b.md", "scheduler-c.md"} {
		content := "---\ntitle: Scheduler kick fact " + string(rune('A'+i)) + "\ntype: pattern\nlayer: project\n---\n" + long
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	store, err := knowledge.NewFileStore(dir, "vault", logger)
	if err != nil {
		t.Fatal(err)
	}
	// 512 is the primer's floor; three ~250-byte snippets cannot all fit.
	p := knowledge.NewPrimer(nil, knowledge.PrimerConfig{MaxBytes: 512}, logger)
	p.AddFileStore("vault", store, knowledge.LayerProject)

	s := New(&config.Config{}, logger)
	s.SetPrimer(p)
	issues := []github.Issue{makeIssue("org/repo", 1, "scheduler kick fact", "", 5, nil, false)}

	section := s.primeKnowledge("developer", issues)
	if section == "" {
		t.Fatal("expected a rendered knowledge section")
	}
	lines := knowledgeLogLines(t, &buf)
	if len(lines) != 1 {
		t.Fatalf("expected one knowledge line per kick, got %d", len(lines))
	}
	rec := lines[0]
	if int(rec["injected_bytes"].(float64)) != len(section) {
		t.Errorf("injected_bytes = %v, want %d", rec["injected_bytes"], len(section))
	}
	if int(rec["cap_bytes"].(float64)) != 512 {
		t.Errorf("cap_bytes = %v, want 512", rec["cap_bytes"])
	}
	entries := int(rec["entries"].(float64))
	skipped := int(rec["skipped_for_cap"].(float64))
	if entries < 1 || skipped < 1 || entries+skipped != 3 {
		t.Errorf("entries=%d skipped=%d, want both ≥1 and summing to 3", entries, skipped)
	}
	if strings.Count(section, "- **Scheduler kick fact") != entries {
		t.Errorf("section has %d entries, log says %d", strings.Count(section, "- **Scheduler kick fact"), entries)
	}
}

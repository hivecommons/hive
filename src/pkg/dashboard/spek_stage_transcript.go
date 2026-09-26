package dashboard

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	spekStageTranscriptSchema       = "spek-stage-transcript/v1"
	spekStageTranscriptMaxTextBytes = 128 * 1024
	spekStagePromptMaxTextBytes     = 32 * 1024
	spekStageFileMaxTextBytes       = 256 * 1024
)

type RunDetailTextBlock struct {
	Text      string `json:"text,omitempty"`
	Bytes     int    `json:"bytes,omitempty"`
	Truncated bool   `json:"truncated,omitempty"`
}

type RunDetailStageStatus struct {
	At             string   `json:"at,omitempty"`
	Step           string   `json:"step,omitempty"`
	DocumentStatus string   `json:"document_status,omitempty"`
	CompletedSteps []string `json:"completed_steps,omitempty"`
	Artifact       string   `json:"artifact,omitempty"`
}

type RunDetailStageFile struct {
	Path      string `json:"path"`
	Bytes     int    `json:"bytes,omitempty"`
	Truncated bool   `json:"truncated,omitempty"`
	Text      string `json:"text,omitempty"`
}

type RunDetailStageDocument struct {
	Path      string `json:"path"`
	Markdown  string `json:"markdown,omitempty"`
	Bytes     int    `json:"bytes,omitempty"`
	Truncated bool   `json:"truncated,omitempty"`
}

type RunDetailInterview struct {
	Step       string `json:"step,omitempty"`
	Question   string `json:"question,omitempty"`
	Answer     string `json:"answer,omitempty"`
	AnsweredAt string `json:"answered_at,omitempty"`
	Source     string `json:"source,omitempty"`
}

type RunDetailStageCapture struct {
	SchemaVersion   string                   `json:"schema_version"`
	RunKey          string                   `json:"run_key,omitempty"`
	Stage           string                   `json:"stage,omitempty"`
	Generation      uint64                   `json:"generation,omitempty"`
	Artifact        string                   `json:"artifact,omitempty"`
	CapturedAt      string                   `json:"captured_at,omitempty"`
	StartedAt       string                   `json:"started_at,omitempty"`
	EndedAt         string                   `json:"ended_at,omitempty"`
	Backend         string                   `json:"backend,omitempty"`
	Model           string                   `json:"model,omitempty"`
	Prompt          *RunDetailTextBlock      `json:"prompt,omitempty"`
	AgentTranscript *RunDetailTextBlock      `json:"agent_transcript,omitempty"`
	StatusHistory   []RunDetailStageStatus   `json:"status_history,omitempty"`
	Interview       []RunDetailInterview     `json:"interview,omitempty"`
	Documents       []RunDetailStageDocument `json:"documents,omitempty"`
	Files           []RunDetailStageFile     `json:"files,omitempty"`
	Notes           []string                 `json:"notes,omitempty"`
}

func spekStageTranscriptFile(stage string, gen uint64) string {
	return fmt.Sprintf("%s-gen%d.transcript.json", stage, gen)
}

func spekStageDocumentFile(stage string, gen uint64) string {
	return fmt.Sprintf("%s-gen%d.%s.md", stage, gen, stage)
}

func writeSpekStageCapture(runKey, stage string, gen uint64, capture RunDetailStageCapture) error {
	return writeSpekStageCaptureInDir(runReceiptsDir, runKey, stage, gen, capture)
}

func writeSpekStageCaptureInDir(receiptsDir, runKey, stage string, gen uint64, capture RunDetailStageCapture) error {
	dir := filepath.Join(receiptsDir, sanitizeReceiptSegment(runKey))
	if err := os.MkdirAll(dir, receiptDirMode); err != nil {
		return err
	}
	capture.SchemaVersion = spekStageTranscriptSchema
	capture.RunKey = firstRunNonEmpty(capture.RunKey, runKey)
	capture.Stage = firstRunNonEmpty(capture.Stage, stage)
	if capture.Generation == 0 {
		capture.Generation = gen
	}
	if capture.CapturedAt == "" {
		capture.CapturedAt = time.Now().UTC().Format(time.RFC3339Nano)
	}
	data, err := json.MarshalIndent(capture, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, spekStageTranscriptFile(stage, gen)), data, receiptFileMode); err != nil {
		return err
	}
	if len(capture.Documents) > 0 && strings.TrimSpace(capture.Documents[0].Markdown) != "" {
		_ = os.WriteFile(filepath.Join(dir, spekStageDocumentFile(stage, gen)), []byte(capture.Documents[0].Markdown), receiptFileMode)
	}
	return nil
}

func readRunDetailStageCaptures(runKeys ...string) []RunDetailStageCapture {
	var out []RunDetailStageCapture
	seen := map[string]bool{}
	for _, runKey := range uniqueRunDetailKeys(runKeys...) {
		dir := filepath.Join(runReceiptsDir, sanitizeReceiptSegment(runKey))
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".transcript.json") {
				continue
			}
			path := filepath.Join(dir, entry.Name())
			if seen[path] {
				continue
			}
			seen[path] = true
			data, err := os.ReadFile(path)
			if err != nil {
				continue
			}
			var cap RunDetailStageCapture
			if json.Unmarshal(data, &cap) != nil || cap.SchemaVersion != spekStageTranscriptSchema {
				continue
			}
			if cap.Stage == "" {
				cap.Stage, cap.Generation = parseTranscriptFile(entry.Name())
			}
			if len(cap.Documents) == 0 {
				if doc, err := os.ReadFile(filepath.Join(dir, spekStageDocumentFile(cap.Stage, cap.Generation))); err == nil {
					cap.Documents = append(cap.Documents, RunDetailStageDocument{Path: spekStageDocumentFile(cap.Stage, cap.Generation), Markdown: string(doc), Bytes: len(doc)})
				}
			}
			out = append(out, cap)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Stage != out[j].Stage {
			return out[i].Stage < out[j].Stage
		}
		return out[i].Generation < out[j].Generation
	})
	return out
}

func parseTranscriptFile(name string) (string, uint64) {
	base := strings.TrimSuffix(name, ".transcript.json")
	stage, raw, ok := strings.Cut(base, "-gen")
	if !ok {
		return "", 0
	}
	gen, _ := strconv.ParseUint(raw, 10, 64)
	return stage, gen
}

func textBlock(data []byte, limit int) RunDetailTextBlock {
	text, truncated := boundedUTF8(data, limit)
	return RunDetailTextBlock{Text: text, Bytes: len(data), Truncated: truncated}
}

func boundedUTF8(data []byte, limit int) (string, bool) {
	truncated := false
	if limit > 0 && len(data) > limit {
		data = data[len(data)-limit:]
		truncated = true
		for len(data) > 0 && !utf8.Valid(data) {
			data = data[1:]
		}
	}
	return string(data), truncated
}

func spekArtifactPaths(worktree, kind, artifact string) []string {
	rootName := "specs"
	if kind == StagePlan {
		rootName = "plans"
	}
	root := filepath.Join(worktree, ".spektacular", rootName)
	var paths []string
	candidates := []string{
		filepath.Join(root, artifact),
		filepath.Join(root, artifact+".md"),
	}
	for _, candidate := range candidates {
		if info, err := os.Stat(candidate); err == nil {
			if info.IsDir() {
				paths = append(paths, candidate)
			} else if !info.IsDir() {
				paths = append(paths, candidate)
			}
		}
	}
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || d == nil {
			return nil
		}
		if d.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		if bareArtifactName(filepath.ToSlash(rel)) == artifact {
			paths = append(paths, path)
		}
		return nil
	})
	seen, out := map[string]bool{}, []string{}
	for _, p := range paths {
		if !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	return out
}

func collectSpekArtifactFiles(worktree, kind, artifact string) ([]RunDetailStageDocument, []RunDetailStageFile, []RunDetailInterview, []string) {
	rootName := "specs"
	if kind == StagePlan {
		rootName = "plans"
	}
	root := filepath.Join(worktree, ".spektacular", rootName)
	var docs []RunDetailStageDocument
	var files []RunDetailStageFile
	var interview []RunDetailInterview
	var notes []string
	for _, base := range spekArtifactPaths(worktree, kind, artifact) {
		_ = filepath.WalkDir(base, func(path string, d os.DirEntry, err error) error {
			if err != nil || d == nil || d.IsDir() {
				return nil
			}
			rel, _ := filepath.Rel(root, path)
			rel = filepath.ToSlash(rel)
			data, err := os.ReadFile(path)
			if err != nil {
				return nil
			}
			text, truncated := boundedUTF8(data, spekStageFileMaxTextBytes)
			file := RunDetailStageFile{Path: rel, Bytes: len(data), Truncated: truncated, Text: text}
			files = append(files, file)
			if strings.HasSuffix(strings.ToLower(rel), ".md") {
				docs = append(docs, RunDetailStageDocument{Path: rel, Markdown: text, Bytes: len(data), Truncated: truncated})
			}
			interview = append(interview, extractInterviewEntries(rel, text)...)
			return nil
		})
	}
	if len(files) == 0 {
		notes = append(notes, "Spektacular artifact files were not found in the executor worktree before capture.")
	}
	sort.Slice(docs, func(i, j int) bool { return docs[i].Path < docs[j].Path })
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	return docs, files, interview, notes
}

func extractInterviewEntries(source, text string) []RunDetailInterview {
	var out []RunDetailInterview
	var pendingQ, pendingStep string
	for _, raw := range strings.Split(text, "\n") {
		line := strings.TrimSpace(strings.Trim(raw, "#-* \t"))
		lower := strings.ToLower(line)
		if strings.HasPrefix(lower, "step ") || strings.Contains(lower, "clarif") || strings.Contains(lower, "interview") {
			pendingStep = line
		}
		if q, ok := labeledValue(line, "question", "instruction", "prompt"); ok {
			pendingQ = q
			if pendingStep == "" {
				pendingStep = firstSentence(q)
			}
			continue
		}
		if a, ok := labeledValue(line, "answer", "response", "reply"); ok {
			out = append(out, RunDetailInterview{Step: pendingStep, Question: pendingQ, Answer: a, Source: source})
			pendingQ, pendingStep = "", ""
		}
	}
	return out
}

func labeledValue(line string, labels ...string) (string, bool) {
	lower := strings.ToLower(line)
	for _, label := range labels {
		prefix := label + ":"
		if strings.HasPrefix(lower, prefix) {
			return strings.TrimSpace(line[len(prefix):]), true
		}
	}
	return "", false
}

func firstSentence(s string) string {
	s = strings.TrimSpace(s)
	if idx := strings.IndexAny(s, ".?!"); idx >= 0 {
		return strings.TrimSpace(s[:idx+1])
	}
	return s
}

package knowledge

import (
	"math"
	"strings"
	"testing"
	"time"
)

func TestScoreConfidenceRescoresLegacyDocDefault(t *testing.T) {
	score, scored, reason := scoreConfidence(confidenceInput{
		Raw:        legacyFlatConfidence,
		HasRaw:     true,
		Type:       FactReference,
		Layer:      LayerProject,
		Tags:       []string{"doc-import", "systemd"},
		Source:     "doc:systemd",
		SourceURL:  "https://www.freedesktop.org/software/systemd/man/latest/",
		SourceDate: time.Now().UTC(),
	})
	if !scored {
		t.Fatal("expected document fact to be scored from provenance")
	}
	if math.Abs(score-legacyFlatConfidence) < 0.0001 {
		t.Fatalf("score stayed at legacy default %.2f; reason=%s", score, reason)
	}
	if reason == "" {
		t.Fatal("expected an operator-facing confidence reason")
	}
}

func TestScoreConfidenceUnscoredWhenNoSignals(t *testing.T) {
	score, scored, reason := scoreConfidence(confidenceInput{})
	if scored {
		t.Fatalf("expected fact without signals to be unscored, got %.2f", score)
	}
	if score != 0 {
		t.Fatalf("unscored confidence = %.2f, want 0", score)
	}
	if reason == "" {
		t.Fatal("expected reason for unscored fact")
	}
}

func TestScoreConfidenceKeepsExplicitNonLegacyDocConfidence(t *testing.T) {
	score, scored, reason := scoreConfidence(confidenceInput{
		Raw:       0.81,
		HasRaw:    true,
		Tags:      []string{"doc-import"},
		SourceURL: "https://example.com/reference",
	})
	if !scored {
		t.Fatal("expected explicit document confidence to remain scored")
	}
	if score != 0.81 {
		t.Fatalf("score = %.2f, want stored confidence 0.81; reason=%s", score, reason)
	}
}

func TestScoreConfidenceScoresHTTPSProvenanceWithoutRawDefault(t *testing.T) {
	score, scored, reason := scoreConfidence(confidenceInput{
		SourceURL: "https://example.com/reference",
		Layer:     LayerProject,
	})
	if !scored {
		t.Fatal("expected HTTPS provenance to be a scoring signal")
	}
	if score <= legacyFlatConfidence {
		t.Fatalf("score = %.2f, want above legacy default; reason=%s", score, reason)
	}
}

func TestScoreConfidenceScoresWorkflowSignalsWithoutStoredConfidence(t *testing.T) {
	score, scored, reason := scoreConfidence(confidenceInput{
		Status:  "verified",
		Layer:   LayerProject,
		Related: []string{"existing-fact"},
	})
	if !scored {
		t.Fatal("expected validation and corroboration signals to score the fact")
	}
	if score <= 0.5 {
		t.Fatalf("score = %.2f, want workflow adjustments above baseline; reason=%s", score, reason)
	}
	if !strings.Contains(reason, "status verified") || !strings.Contains(reason, "related") {
		t.Fatalf("reason should explain validation and corroboration signals, got %q", reason)
	}
}

func TestFileStoreRescoresExistingDocAndKeepsExplicitConfidence(t *testing.T) {
	dir := t.TempDir()
	writeMarkdown(t, dir, "doc-systemd.md", `---
title: systemd service files
type: reference
layer: project
confidence: 0.60
tags: [doc-import, systemd]
source: doc:systemd
source_url: https://www.freedesktop.org/software/systemd/man/latest/
synthesized: `+time.Now().UTC().Format(time.RFC3339)+`
---
Unit files describe services.`)
	writeMarkdown(t, dir, "manual.md", `---
title: Manual validation
confidence: 0.91
---
Human-entered fact.`)

	store, err := NewFileStore(dir, "test", fileStoreTestLogger())
	if err != nil {
		t.Fatal(err)
	}
	doc, err := store.ReadPage("doc-systemd")
	if err != nil {
		t.Fatal(err)
	}
	if !doc.ConfidenceScored {
		t.Fatal("expected existing document fact to be rescored on read")
	}
	if math.Abs(doc.Confidence-legacyFlatConfidence) < 0.0001 {
		t.Fatalf("document fact remained at legacy 60%% default")
	}
	if doc.ConfidenceReason == "" {
		t.Fatal("expected confidence reason for document fact")
	}

	manual, err := store.ReadPage("manual")
	if err != nil {
		t.Fatal(err)
	}
	if manual.Confidence != 0.91 {
		t.Fatalf("manual confidence = %.2f, want 0.91", manual.Confidence)
	}
	if !manual.ConfidenceScored {
		t.Fatal("explicit manual confidence should remain scored")
	}
}

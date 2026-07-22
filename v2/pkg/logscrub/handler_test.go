package logscrub

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
)

func TestScrubsGitHubTokenInMessage(t *testing.T) {
	var buf bytes.Buffer
	inner := slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})
	logger := slog.New(NewHandler(inner))

	logger.Info("token is ghs_abc1234567890XYZ")
	if strings.Contains(buf.String(), "ghs_") {
		t.Errorf("token not scrubbed from message: %s", buf.String())
	}
	if !strings.Contains(buf.String(), "[REDACTED]") {
		t.Errorf("expected [REDACTED] in output: %s", buf.String())
	}
}

func TestScrubsGitHubTokenInAttr(t *testing.T) {
	var buf bytes.Buffer
	inner := slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})
	logger := slog.New(NewHandler(inner))

	logger.Info("auth loaded", "token", "gh"+"p_secretvalue1234567890")
	if strings.Contains(buf.String(), "ghp_") {
		t.Errorf("token not scrubbed from attr: %s", buf.String())
	}
}

func TestScrubsJWT(t *testing.T) {
	var buf bytes.Buffer
	inner := slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})
	logger := slog.New(NewHandler(inner))

	jwt := "eyJhbGciOiJSUzI1NiIsInR5cCI6IkpXVCJ9.eyJpYXQiOjE2MDAwMDAwMDAsImV4cCI6MTYwMDAwMDYwMH0.SflKxwRJSMeKKF2QT4fwpMeJf36POk6yJV_adQssw5c"
	logger.Info("jwt used", "value", jwt)
	if strings.Contains(buf.String(), "eyJhbGci") {
		t.Errorf("JWT not scrubbed: %s", buf.String())
	}
}

func TestPassesThroughNonSecrets(t *testing.T) {
	var buf bytes.Buffer
	inner := slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})
	logger := slog.New(NewHandler(inner))

	logger.Info("normal message", "key", "normal_value", "count", 42)
	if !strings.Contains(buf.String(), "normal message") {
		t.Errorf("message was altered: %s", buf.String())
	}
	if !strings.Contains(buf.String(), "normal_value") {
		t.Errorf("non-secret attr was altered: %s", buf.String())
	}
}

func TestScrubsMultiplePatterns(t *testing.T) {
	var buf bytes.Buffer
	inner := slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})
	logger := slog.New(NewHandler(inner))

	logger.Info("tokens: " + "gh" + "o_oauthtoken12345678 and github" + "_pat_longpersonaltoken1234567890")
	if strings.Contains(buf.String(), "gho_") || strings.Contains(buf.String(), "github_pat_") {
		t.Errorf("not all tokens scrubbed: %s", buf.String())
	}
}

func TestWithAttrs(t *testing.T) {
	var buf bytes.Buffer
	inner := slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})
	logger := slog.New(NewHandler(inner)).With("secret", "gh"+"s_presettoken1234567890")

	logger.Info("test")
	if strings.Contains(buf.String(), "ghs_") {
		t.Errorf("token in WithAttrs not scrubbed: %s", buf.String())
	}
}

func TestEnabled(t *testing.T) {
	inner := slog.NewTextHandler(&bytes.Buffer{}, &slog.HandlerOptions{Level: slog.LevelWarn})
	h := NewHandler(inner)

	if h.Enabled(context.Background(), slog.LevelInfo) {
		t.Error("should not be enabled for INFO when inner is WARN")
	}
	if !h.Enabled(context.Background(), slog.LevelWarn) {
		t.Error("should be enabled for WARN")
	}
}

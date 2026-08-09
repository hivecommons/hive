package hub

import (
	"strings"
	"testing"
)

// TestSanitizePublicURLSelfCheck covers sanitizePublicURLSelfCheck which
// validates and sanitises spoke-reported public-URL self-check payloads.
func TestSanitizePublicURLSelfCheck(t *testing.T) {
	t.Run("nil input returns nil", func(t *testing.T) {
		if got := sanitizePublicURLSelfCheck(nil); got != nil {
			t.Errorf("expected nil, got %+v", got)
		}
	})

	t.Run("valid status ok", func(t *testing.T) {
		in := &PublicURLSelfCheck{
			Status:     "ok",
			CheckedAt:  "2026-08-09T12:00:00Z",
			Error:      "",
			HTTPStatus: 200,
		}
		got := sanitizePublicURLSelfCheck(in)
		if got == nil {
			t.Fatal("expected non-nil result")
		}
		if got.Status != PublicURLSelfCheckOK {
			t.Errorf("Status = %q, want %q", got.Status, PublicURLSelfCheckOK)
		}
		if got.HTTPStatus != 200 {
			t.Errorf("HTTPStatus = %d, want 200", got.HTTPStatus)
		}
	})

	t.Run("valid status fail with error", func(t *testing.T) {
		in := &PublicURLSelfCheck{
			Status:     "fail",
			CheckedAt:  "2026-08-09T12:00:00Z",
			Error:      "connection refused",
			HTTPStatus: 0,
		}
		got := sanitizePublicURLSelfCheck(in)
		if got == nil {
			t.Fatal("expected non-nil result")
		}
		if got.Status != PublicURLSelfCheckFail {
			t.Errorf("Status = %q, want %q", got.Status, PublicURLSelfCheckFail)
		}
		if got.Error != "connection refused" {
			t.Errorf("Error = %q, want %q", got.Error, "connection refused")
		}
	})

	t.Run("valid status unknown", func(t *testing.T) {
		in := &PublicURLSelfCheck{
			Status:    "unknown",
			CheckedAt: "2026-08-09T12:00:00Z",
		}
		got := sanitizePublicURLSelfCheck(in)
		if got == nil {
			t.Fatal("expected non-nil result")
		}
		if got.Status != PublicURLSelfCheckUnknown {
			t.Errorf("Status = %q, want %q", got.Status, PublicURLSelfCheckUnknown)
		}
	})

	t.Run("invalid status returns nil", func(t *testing.T) {
		cases := []string{"", "OK", "FAIL", "bogus", "<script>", "ok ok"}
		for _, status := range cases {
			in := &PublicURLSelfCheck{Status: status, CheckedAt: "2026-08-09T12:00:00Z"}
			if got := sanitizePublicURLSelfCheck(in); got != nil {
				t.Errorf("status %q: expected nil, got %+v", status, got)
			}
		}
	})

	t.Run("HTTPStatus clamped to 0-599", func(t *testing.T) {
		in := &PublicURLSelfCheck{
			Status:     "ok",
			HTTPStatus: 999,
		}
		got := sanitizePublicURLSelfCheck(in)
		if got == nil {
			t.Fatal("expected non-nil result")
		}
		if got.HTTPStatus != 599 {
			t.Errorf("HTTPStatus = %d, want 599 (clamped)", got.HTTPStatus)
		}

		in2 := &PublicURLSelfCheck{
			Status:     "fail",
			HTTPStatus: -1,
		}
		got2 := sanitizePublicURLSelfCheck(in2)
		if got2 == nil {
			t.Fatal("expected non-nil result")
		}
		if got2.HTTPStatus != 0 {
			t.Errorf("HTTPStatus = %d, want 0 (clamped)", got2.HTTPStatus)
		}
	})

	t.Run("fields are sanitized", func(t *testing.T) {
		in := &PublicURLSelfCheck{
			Status:     "fail",
			CheckedAt:  "2026-08-09T12:00:00Z<script>alert(1)</script>",
			Error:      "timeout <b>connecting</b> to\nlocalhost",
			HTTPStatus: 503,
		}
		got := sanitizePublicURLSelfCheck(in)
		if got == nil {
			t.Fatal("expected non-nil result")
		}
		// CheckedAt goes through sanitizeHeartbeatField — strips non-identifier chars
		if got.CheckedAt != "2026-08-09T120000Zscriptalert1/script" {
			t.Errorf("CheckedAt not sanitized correctly: %q", got.CheckedAt)
		}
		// Error goes through sanitizeProseField — strips HTML metacharacters
		if got.Error == in.Error {
			t.Error("Error field was not sanitized")
		}
		// HTML tags must be stripped
		for _, bad := range []string{"<", ">", "\n"} {
			if strContains(got.Error, bad) {
				t.Errorf("Error still contains %q: %q", bad, got.Error)
			}
		}
	})

	t.Run("does not alias input pointer", func(t *testing.T) {
		in := &PublicURLSelfCheck{Status: "ok", HTTPStatus: 200}
		got := sanitizePublicURLSelfCheck(in)
		if got == in {
			t.Error("returned pointer aliases input — must be a fresh allocation")
		}
	})
}

// TestSanitizeRouteExistenceCheck covers sanitizeRouteExistenceCheck which
// validates and sanitises spoke-reported route-existence check payloads.
func TestSanitizeRouteExistenceCheck(t *testing.T) {
	t.Run("nil input returns nil", func(t *testing.T) {
		if got := sanitizeRouteExistenceCheck(nil); got != nil {
			t.Errorf("expected nil, got %+v", got)
		}
	})

	t.Run("valid status found", func(t *testing.T) {
		in := &RouteExistenceCheck{
			Status:    "found",
			CheckedAt: "2026-08-09T12:00:00Z",
			Host:      "hive.example.com",
			Kind:      "Ingress",
		}
		got := sanitizeRouteExistenceCheck(in)
		if got == nil {
			t.Fatal("expected non-nil result")
		}
		if got.Status != RouteExistenceFound {
			t.Errorf("Status = %q, want %q", got.Status, RouteExistenceFound)
		}
		if got.Host != "hive.example.com" {
			t.Errorf("Host = %q, want %q", got.Host, "hive.example.com")
		}
		if got.Kind != "Ingress" {
			t.Errorf("Kind = %q, want %q", got.Kind, "Ingress")
		}
	})

	t.Run("valid status missing", func(t *testing.T) {
		in := &RouteExistenceCheck{
			Status:    "missing",
			CheckedAt: "2026-08-09T12:00:00Z",
			Host:      "hive.example.com",
			Kind:      "Route",
			Error:     "no matching resource",
		}
		got := sanitizeRouteExistenceCheck(in)
		if got == nil {
			t.Fatal("expected non-nil result")
		}
		if got.Status != RouteExistenceMissing {
			t.Errorf("Status = %q, want %q", got.Status, RouteExistenceMissing)
		}
	})

	t.Run("valid status unknown", func(t *testing.T) {
		in := &RouteExistenceCheck{
			Status: "unknown",
			Error:  "RBAC denied",
		}
		got := sanitizeRouteExistenceCheck(in)
		if got == nil {
			t.Fatal("expected non-nil result")
		}
		if got.Status != RouteExistenceUnknown {
			t.Errorf("Status = %q, want %q", got.Status, RouteExistenceUnknown)
		}
	})

	t.Run("invalid status returns nil", func(t *testing.T) {
		cases := []string{"", "FOUND", "Missing", "present", "<img>", "found found"}
		for _, status := range cases {
			in := &RouteExistenceCheck{Status: status, Host: "x.com"}
			if got := sanitizeRouteExistenceCheck(in); got != nil {
				t.Errorf("status %q: expected nil, got %+v", status, got)
			}
		}
	})

	t.Run("fields are sanitized", func(t *testing.T) {
		in := &RouteExistenceCheck{
			Status:    "found",
			CheckedAt: "2026-08-09T12:00:00Z  extra",
			Host:      "hive.example.com<script>",
			Kind:      "Ingress; DROP TABLE",
			Error:     "no error <b>here</b>",
		}
		got := sanitizeRouteExistenceCheck(in)
		if got == nil {
			t.Fatal("expected non-nil result")
		}
		// CheckedAt, Host, Kind go through sanitizeHeartbeatField
		for _, bad := range []string{"<", ">", " ", ";"} {
			if strContains(got.CheckedAt, bad) {
				t.Errorf("CheckedAt still contains %q: %q", bad, got.CheckedAt)
			}
			if strContains(got.Host, bad) {
				t.Errorf("Host still contains %q: %q", bad, got.Host)
			}
			if strContains(got.Kind, bad) {
				t.Errorf("Kind still contains %q: %q", bad, got.Kind)
			}
		}
		// Error goes through sanitizeProseField
		for _, bad := range []string{"<", ">"} {
			if strContains(got.Error, bad) {
				t.Errorf("Error still contains %q: %q", bad, got.Error)
			}
		}
	})

	t.Run("does not alias input pointer", func(t *testing.T) {
		in := &RouteExistenceCheck{Status: "found", Host: "x.com"}
		got := sanitizeRouteExistenceCheck(in)
		if got == in {
			t.Error("returned pointer aliases input — must be a fresh allocation")
		}
	})
}

// Use strings.Contains via the helper below to avoid redeclaring contains.
func strContains(s, sub string) bool { return strings.Contains(s, sub) }

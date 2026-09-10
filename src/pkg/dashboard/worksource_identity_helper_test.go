package dashboard

import (
	"log/slog"
	"testing"

	ghpkg "github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/worksource"
)

// Focused unit tests for forEachActionableIssue: the single place the
// []any -> map[string]any conversion and the #4245 identity/skip contract
// ("skip only when no number AND no external key") now live, shared by
// selectTask (contribute_ws.go) and admissionQueueSnapshot (contribute_sse.go).

func TestForEachActionableIssue_NumberOnlyIsVisited(t *testing.T) {
	raw := []any{
		map[string]any{"number": float64(42), "title": "number only"},
	}
	var got []worksource.Ref
	forEachActionableIssue(nil, "", "acme/repo", raw, func(issue map[string]any, ref worksource.Ref) {
		got = append(got, ref)
	})
	if len(got) != 1 {
		t.Fatalf("expected 1 visited item, got %d", len(got))
	}
	if got[0].Number != 42 || got[0].Key() == "" {
		t.Fatalf("expected a usable number-based ref, got %+v (key=%q)", got[0], got[0].Key())
	}
}

func TestForEachActionableIssue_ExternalKeyOnlyIsVisited(t *testing.T) {
	raw := []any{
		map[string]any{
			"number":      float64(0),
			"source_type": "linear",
			"external_id": "ACME-123",
			"title":       "external only",
		},
	}
	var got []worksource.Ref
	forEachActionableIssue(nil, "", "acme/repo", raw, func(issue map[string]any, ref worksource.Ref) {
		got = append(got, ref)
	})
	if len(got) != 1 {
		t.Fatalf("expected 1 visited item, got %d", len(got))
	}
	if got[0].ExternalID != "ACME-123" || got[0].Key() == "" {
		t.Fatalf("expected a usable external-id-based ref, got %+v (key=%q)", got[0], got[0].Key())
	}
}

func TestForEachActionableIssue_NoIdentityIsSkipped(t *testing.T) {
	raw := []any{
		map[string]any{"number": float64(0), "title": "no identity at all"},
	}
	visited := false
	forEachActionableIssue(nil, "", "acme/repo", raw, func(issue map[string]any, ref worksource.Ref) {
		visited = true
	})
	if visited {
		t.Fatal("expected item with no number and no external key to be skipped, but fn was called")
	}
}

func TestForEachActionableIssue_StructInputSameAsMapInput(t *testing.T) {
	// ActionableIssues holds ghpkg.Issue structs before the status payload's
	// JSON round-trip strips them down to maps; both shapes must produce the
	// same identity.
	structItem := ghpkg.Issue{Repo: "acme/repo", Number: 7, Title: "struct form"}
	mapItem := map[string]any{"number": float64(7), "title": "map form"}

	var structRef, mapRef worksource.Ref
	forEachActionableIssue(nil, "", "acme/repo", []any{structItem}, func(issue map[string]any, ref worksource.Ref) {
		structRef = ref
	})
	forEachActionableIssue(nil, "", "acme/repo", []any{mapItem}, func(issue map[string]any, ref worksource.Ref) {
		mapRef = ref
	})
	if structRef.Key() == "" || mapRef.Key() == "" {
		t.Fatalf("expected both struct and map input to yield a usable ref, got struct=%+v map=%+v", structRef, mapRef)
	}
	if structRef.Key() != mapRef.Key() {
		t.Fatalf("expected struct and map input for the same issue to yield the same key, got %q vs %q", structRef.Key(), mapRef.Key())
	}
}

func TestForEachActionableIssue_MultipleItemsPreserveOrderAndSkipInPlace(t *testing.T) {
	raw := []any{
		map[string]any{"number": float64(1)},
		map[string]any{"number": float64(0)}, // no identity: skipped
		map[string]any{"number": float64(2)},
	}
	var numbers []int
	forEachActionableIssue(nil, "", "acme/repo", raw, func(issue map[string]any, ref worksource.Ref) {
		numbers = append(numbers, ref.Number)
	})
	if len(numbers) != 2 || numbers[0] != 1 || numbers[1] != 2 {
		t.Fatalf("expected [1 2] with the no-identity item skipped in place, got %v", numbers)
	}
}

func TestForEachActionableIssue_LoggerOptional(t *testing.T) {
	// A nil logger must not panic even when an item is skipped for lacking
	// identity, or when an entry fails to marshal.
	raw := []any{
		map[string]any{"number": float64(0)},
		make(chan int), // unmarshalable: json.Marshal fails
	}
	forEachActionableIssue(nil, "", "acme/repo", raw, func(issue map[string]any, ref worksource.Ref) {
		t.Fatal("fn must not be invoked for a skipped or unmarshalable entry")
	})

	// A non-nil logger takes the same path without panicking either.
	forEachActionableIssue(slog.Default(), "[test]", "acme/repo", raw, func(issue map[string]any, ref worksource.Ref) {
		t.Fatal("fn must not be invoked for a skipped or unmarshalable entry")
	})
}

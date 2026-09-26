package worksource

import (
	"context"
	"errors"
	"testing"
)

type compositeMutationSource struct {
	sourceType string
	calls      []string
}

func (s *compositeMutationSource) SourceType() string { return s.sourceType }

func (s *compositeMutationSource) ListIssues(context.Context) ([]Issue, error) {
	return nil, nil
}

func (s *compositeMutationSource) AddLabel(_ context.Context, _ Ref, label string) error {
	s.calls = append(s.calls, "add:"+label)
	return nil
}

func (s *compositeMutationSource) RemoveLabel(_ context.Context, _ Ref, label string) error {
	s.calls = append(s.calls, "remove:"+label)
	return nil
}

func (s *compositeMutationSource) AddComment(_ context.Context, _ Ref, body string) error {
	s.calls = append(s.calls, "comment:"+body)
	return nil
}

func (s *compositeMutationSource) TransitionStatus(_ context.Context, _ Ref, status string) error {
	s.calls = append(s.calls, "status:"+status)
	return nil
}

type compositeReadOnlySource struct{ sourceType string }

func (s compositeReadOnlySource) SourceType() string { return s.sourceType }

func (s compositeReadOnlySource) ListIssues(context.Context) ([]Issue, error) {
	return nil, nil
}

func TestCompositeDesignMutationsForwardToPrimary(t *testing.T) {
	primary := &compositeMutationSource{sourceType: "jira"}
	c := NewComposite(primary)
	ref := Ref{Repo: "acme/app", ExternalID: "ENG-7"}
	if err := c.AddLabel(context.Background(), ref, "hive-design"); err != nil {
		t.Fatalf("AddLabel: %v", err)
	}
	if err := c.RemoveLabel(context.Background(), ref, "hive-design"); err != nil {
		t.Fatalf("RemoveLabel: %v", err)
	}
	if err := c.AddComment(context.Background(), ref, "design"); err != nil {
		t.Fatalf("AddComment: %v", err)
	}
	if err := c.TransitionStatus(context.Background(), ref, "approved"); err != nil {
		t.Fatalf("TransitionStatus: %v", err)
	}
	want := []string{"add:hive-design", "remove:hive-design", "comment:design", "status:approved"}
	if len(primary.calls) != len(want) {
		t.Fatalf("calls = %v, want %v", primary.calls, want)
	}
	for i := range want {
		if primary.calls[i] != want[i] {
			t.Fatalf("calls = %v, want %v", primary.calls, want)
		}
	}
}

func TestCompositeDesignMutationUnsupported(t *testing.T) {
	c := NewComposite(compositeReadOnlySource{sourceType: "readonly"})
	if err := c.AddLabel(context.Background(), Ref{}, "x"); err == nil {
		t.Fatal("AddLabel returned nil for readonly source")
	}
	if err := c.RemoveLabel(context.Background(), Ref{}, "x"); err == nil {
		t.Fatal("RemoveLabel returned nil for readonly source")
	}
	if err := c.AddComment(context.Background(), Ref{}, "body"); err == nil {
		t.Fatal("AddComment returned nil for readonly source")
	}
	if err := c.TransitionStatus(context.Background(), Ref{}, "done"); !errors.Is(err, ErrStatusTransitionUnsupported) {
		t.Fatalf("TransitionStatus err = %v", err)
	}
}

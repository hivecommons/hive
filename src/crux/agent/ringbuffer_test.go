package agent

import (
	"fmt"
	"testing"
)

func TestRingBuffer_BasicWriteRead(t *testing.T) {
	rb := NewRingBuffer(5)

	rb.Write("a")
	rb.Write("b")
	rb.Write("c")

	got := rb.Last(3)
	want := []string{"a", "b", "c"}
	if len(got) != len(want) {
		t.Fatalf("got %d items, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("item %d: got %q, want %q", i, got[i], want[i])
		}
	}
}

func TestRingBuffer_Overflow(t *testing.T) {
	rb := NewRingBuffer(3)

	rb.Write("a")
	rb.Write("b")
	rb.Write("c")
	rb.Write("d")
	rb.Write("e")

	got := rb.Last(3)
	want := []string{"c", "d", "e"}
	if len(got) != len(want) {
		t.Fatalf("got %d items, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("item %d: got %q, want %q", i, got[i], want[i])
		}
	}
}

func TestRingBuffer_LastMoreThanCount(t *testing.T) {
	rb := NewRingBuffer(10)
	rb.Write("a")
	rb.Write("b")

	got := rb.Last(100)
	if len(got) != 2 {
		t.Fatalf("got %d items, want 2", len(got))
	}
}

func TestRingBuffer_Empty(t *testing.T) {
	rb := NewRingBuffer(5)
	got := rb.Last(10)
	if got != nil {
		t.Fatalf("expected nil for empty buffer, got %v", got)
	}
}

func TestRingBuffer_Count(t *testing.T) {
	rb := NewRingBuffer(3)
	if rb.Count() != 0 {
		t.Fatalf("expected 0, got %d", rb.Count())
	}

	rb.Write("a")
	rb.Write("b")
	if rb.Count() != 2 {
		t.Fatalf("expected 2, got %d", rb.Count())
	}

	rb.Write("c")
	rb.Write("d")
	if rb.Count() != 3 {
		t.Fatalf("expected 3 (capped), got %d", rb.Count())
	}
}

func TestRingBuffer_DefaultCapacity(t *testing.T) {
	rb := NewRingBuffer(0)
	if rb.cap != defaultRingCapacity {
		t.Fatalf("expected default capacity %d, got %d", defaultRingCapacity, rb.cap)
	}
}

func TestRingBuffer_LargeDataSet(t *testing.T) {
	const cap = 100
	rb := NewRingBuffer(cap)

	const totalWrites = 1000
	for i := range totalWrites {
		rb.Write(fmt.Sprintf("line-%d", i))
	}

	got := rb.Last(cap)
	if len(got) != cap {
		t.Fatalf("got %d items, want %d", len(got), cap)
	}

	expectedFirst := fmt.Sprintf("line-%d", totalWrites-cap)
	if got[0] != expectedFirst {
		t.Errorf("first item: got %q, want %q", got[0], expectedFirst)
	}

	expectedLast := fmt.Sprintf("line-%d", totalWrites-1)
	if got[cap-1] != expectedLast {
		t.Errorf("last item: got %q, want %q", got[cap-1], expectedLast)
	}
}

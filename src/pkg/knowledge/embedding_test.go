package knowledge

import (
	"math"
	"testing"
)

func TestTFEmbedder_Deterministic(t *testing.T) {
	e := NewTFEmbedder(EmbeddingDim)
	a := e.Embed("hello world")
	b := e.Embed("hello world")

	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("embeddings should be deterministic; differ at dim %d", i)
		}
	}
}

func TestTFEmbedder_Dimension(t *testing.T) {
	for _, dim := range []int{64, 128, 256} {
		e := NewTFEmbedder(dim)
		vec := e.Embed("test embedding dimension")
		if len(vec) != dim {
			t.Errorf("dim=%d: expected length %d, got %d", dim, dim, len(vec))
		}
	}
}

func TestTFEmbedder_Normalized(t *testing.T) {
	e := NewTFEmbedder(EmbeddingDim)
	vec := e.Embed("the quick brown fox jumps over the lazy dog")

	var norm float64
	for _, v := range vec {
		norm += v * v
	}
	norm = math.Sqrt(norm)

	if math.Abs(norm-1.0) > 1e-6 {
		t.Errorf("embedding should be L2-normalized: norm=%f", norm)
	}
}

func TestTFEmbedder_SimilarTextHighCosine(t *testing.T) {
	e := NewTFEmbedder(EmbeddingDim)
	a := e.Embed("kubernetes pod scheduling")
	b := e.Embed("kubernetes pod scheduling policy")
	c := e.Embed("chocolate cake recipe baking")

	simAB := CosineSimilarity(a, b)
	simAC := CosineSimilarity(a, c)

	if simAB <= simAC {
		t.Errorf("similar texts should have higher cosine: sim(k8s,k8s')=%f <= sim(k8s,cake)=%f", simAB, simAC)
	}
}

func TestTFEmbedder_EmptyText(t *testing.T) {
	e := NewTFEmbedder(EmbeddingDim)
	vec := e.Embed("")
	if len(vec) != EmbeddingDim {
		t.Errorf("empty text should still produce correct dimension: got %d", len(vec))
	}
	// All zeros after normalization of zero vector.
	for _, v := range vec {
		if v != 0 {
			t.Error("empty text embedding should be zero vector")
			break
		}
	}
}

func TestTFEmbedder_DefaultDim(t *testing.T) {
	e := NewTFEmbedder(0)
	vec := e.Embed("test")
	if len(vec) != EmbeddingDim {
		t.Errorf("dim=0 should default to %d, got %d", EmbeddingDim, len(vec))
	}
}

func TestEmbeddingCache_CachesResults(t *testing.T) {
	calls := 0
	mock := &mockEmbedder{fn: func(text string) []float64 {
		calls++
		return []float64{1, 0, 0}
	}}

	cache := NewEmbeddingCache(mock)
	cache.Embed("hello")
	cache.Embed("hello")
	cache.Embed("hello")

	if calls != 1 {
		t.Errorf("expected 1 call to underlying embedder, got %d", calls)
	}
}

func TestEmbeddingCache_Clear(t *testing.T) {
	calls := 0
	mock := &mockEmbedder{fn: func(text string) []float64 {
		calls++
		return []float64{1, 0, 0}
	}}

	cache := NewEmbeddingCache(mock)
	cache.Embed("hello")
	cache.Clear()
	cache.Embed("hello")

	if calls != 2 {
		t.Errorf("after Clear, should re-embed: expected 2 calls, got %d", calls)
	}
}

func TestTokenize(t *testing.T) {
	tokens := tokenize("Hello, World! foo-bar_baz 123")
	expected := []string{"hello", "world", "foo", "bar", "baz", "123"}

	if len(tokens) != len(expected) {
		t.Fatalf("expected %d tokens, got %d: %v", len(expected), len(tokens), tokens)
	}
	for i, tok := range tokens {
		if tok != expected[i] {
			t.Errorf("token[%d]: expected %q, got %q", i, expected[i], tok)
		}
	}
}

type mockEmbedder struct {
	fn func(string) []float64
}

func (m *mockEmbedder) Embed(text string) []float64 { return m.fn(text) }

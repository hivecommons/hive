package knowledge

import (
	"math"
	"strings"
	"sync"
	"unicode"
)

// EmbeddingDim is the dimensionality of term-frequency embeddings.
// Larger values reduce hash collisions but cost more memory per fact.
const EmbeddingDim = 256

// Embedder produces fixed-length vector representations of text.
// The default implementation uses hashed term-frequency vectors,
// which need no external model. Replace with a real embedding provider
// (e.g. nomic-embed via Ollama) for higher quality.
type Embedder interface {
	Embed(text string) []float64
}

// TFEmbedder produces term-frequency vectors projected into a fixed-size
// space via feature hashing (the "hashing trick"). No external dependencies.
type TFEmbedder struct {
	dim int
}

// NewTFEmbedder creates a term-frequency embedder with the given dimensionality.
func NewTFEmbedder(dim int) *TFEmbedder {
	if dim <= 0 {
		dim = EmbeddingDim
	}
	return &TFEmbedder{dim: dim}
}

// Embed converts text to a fixed-length TF vector via feature hashing,
// then L2-normalizes.
func (e *TFEmbedder) Embed(text string) []float64 {
	tokens := tokenize(text)
	vec := make([]float64, e.dim)

	for _, tok := range tokens {
		h := fnv1a(tok)
		idx := h % uint64(e.dim)
		// Signed hashing: second hash bit determines +1/-1 to reduce bias.
		if (h>>32)&1 == 0 {
			vec[idx]++
		} else {
			vec[idx]--
		}
	}

	l2Normalize(vec)
	return vec
}

// EmbeddingCache caches embeddings by text to avoid recomputation during
// a single search pass. Not persisted — rebuilt each time the store reindexes.
type EmbeddingCache struct {
	embedder Embedder
	mu       sync.RWMutex
	cache    map[string][]float64
}

// NewEmbeddingCache wraps an embedder with an in-memory cache.
func NewEmbeddingCache(embedder Embedder) *EmbeddingCache {
	return &EmbeddingCache{
		embedder: embedder,
		cache:    make(map[string][]float64),
	}
}

// Embed returns a cached embedding or computes and caches one.
func (c *EmbeddingCache) Embed(text string) []float64 {
	c.mu.RLock()
	if vec, ok := c.cache[text]; ok {
		c.mu.RUnlock()
		return vec
	}
	c.mu.RUnlock()

	vec := c.embedder.Embed(text)

	c.mu.Lock()
	c.cache[text] = vec
	c.mu.Unlock()

	return vec
}

// Clear drops all cached embeddings (call after reindex).
func (c *EmbeddingCache) Clear() {
	c.mu.Lock()
	c.cache = make(map[string][]float64)
	c.mu.Unlock()
}

// tokenize splits text into lowercase word tokens, stripping punctuation.
func tokenize(text string) []string {
	lower := strings.ToLower(text)
	words := strings.FieldsFunc(lower, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
	return words
}

// fnv1a computes a 64-bit FNV-1a hash of a string.
func fnv1a(s string) uint64 {
	const (
		offset64 = 14695981039346656037
		prime64  = 1099511628211
	)
	h := uint64(offset64)
	for i := 0; i < len(s); i++ {
		h ^= uint64(s[i])
		h *= prime64
	}
	return h
}

// l2Normalize normalizes a vector in-place to unit length.
func l2Normalize(vec []float64) {
	var norm float64
	for _, v := range vec {
		norm += v * v
	}
	norm = math.Sqrt(norm)
	if norm < minSigma {
		return
	}
	for i := range vec {
		vec[i] /= norm
	}
}

package knowledge

import "math"

// Fisher-Rao geodesic distance on the manifold of diagonal Gaussian distributions.
//
// For two diagonal Gaussians N(μ₁,Σ₁) and N(μ₂,Σ₂) where Σ = diag(σ²),
// the Fisher information metric induces a Riemannian structure. The geodesic
// distance between them decomposes into independent 1D contributions per
// dimension, each computed on the upper half-plane model H² = {(μ,σ) : σ>0}
// with metric ds² = (dμ² + 2dσ²) / σ².
//
// Reference: Pinele, Strapasson & Costa, "Fisher–Rao distance for Gaussians,"
// arXiv:1903.09225. SLM (arXiv:2603.14588) applies this to agent memory retrieval.

const (
	// Minimum standard deviation to avoid division by zero.
	minSigma = 1e-8

	// Number of accesses before transitioning from cosine to Fisher-Rao.
	// Below this threshold, cosine similarity is faster and sufficient for
	// cold-start facts. Above it, the richer geometric distance improves ranking.
	fisherRaoAccessThreshold = 10

	// Blending weight ceiling — Fisher-Rao contribution maxes out at this value.
	// The remaining (1-maxFRWeight) always comes from cosine, keeping the score
	// grounded in a familiar metric.
	maxFRWeight = 0.7
)

// GaussianParams represents a diagonal Gaussian in embedding space.
// Mean is the embedding vector; Sigma is per-dimension standard deviation
// estimated from usage variance.
type GaussianParams struct {
	Mean  []float64
	Sigma []float64
}

// NewGaussianFromEmbedding creates Gaussian params from a raw embedding vector.
// Initial sigma is set to defaultSigma for all dimensions (no variance observed yet).
func NewGaussianFromEmbedding(embedding []float64, defaultSigma float64) GaussianParams {
	sigma := make([]float64, len(embedding))
	for i := range sigma {
		sigma[i] = defaultSigma
	}
	return GaussianParams{Mean: embedding, Sigma: sigma}
}

// FisherRaoDistance computes the geodesic distance between two diagonal
// Gaussians on the Fisher information manifold. Each dimension contributes
// independently via the hyperbolic distance on the upper half-plane.
func FisherRaoDistance(a, b GaussianParams) float64 {
	n := len(a.Mean)
	if n == 0 || n != len(b.Mean) || n != len(a.Sigma) || n != len(b.Sigma) {
		return math.Inf(1)
	}

	var sumSq float64
	for i := 0; i < n; i++ {
		d := halfPlaneDistance(a.Mean[i], clampSigma(a.Sigma[i]), b.Mean[i], clampSigma(b.Sigma[i]))
		sumSq += d * d
	}
	return math.Sqrt(sumSq)
}

// halfPlaneDistance computes the geodesic distance between two points
// (μ₁,σ₁) and (μ₂,σ₂) on the upper half-plane H² with the Fisher metric.
//
// d(p₁,p₂) = sqrt(2) · arccosh(1 + ((μ₁-μ₂)² + 2(σ₁-σ₂)²) / (4·σ₁·σ₂))
func halfPlaneDistance(mu1, sigma1, mu2, sigma2 float64) float64 {
	dmu := mu1 - mu2
	dsig := sigma1 - sigma2
	numerator := dmu*dmu + 2*dsig*dsig
	denominator := 4 * sigma1 * sigma2
	arg := 1.0 + numerator/denominator
	return math.Sqrt2 * math.Acosh(arg)
}

// CosineSimilarity computes the cosine similarity between two vectors.
// Returns 0 for zero-length or mismatched vectors.
func CosineSimilarity(a, b []float64) float64 {
	if len(a) != len(b) || len(a) == 0 {
		return 0
	}

	var dot, normA, normB float64
	for i := range a {
		dot += a[i] * b[i]
		normA += a[i] * a[i]
		normB += b[i] * b[i]
	}

	denom := math.Sqrt(normA) * math.Sqrt(normB)
	if denom < minSigma {
		return 0
	}
	return dot / denom
}

// GraduatedScore blends cosine similarity and Fisher-Rao distance based on
// how many times the fact has been accessed. Cold facts use pure cosine;
// well-established facts get Fisher-Rao's richer geometry.
//
// The score is always in [0, 1] where 1 = perfect match.
func GraduatedScore(query, fact GaussianParams, accessCount int) float64 {
	cosine := CosineSimilarity(query.Mean, fact.Mean)

	if accessCount < fisherRaoAccessThreshold {
		return clampScore(cosine)
	}

	frDist := FisherRaoDistance(query, fact)
	frSim := frDistanceToSimilarity(frDist)

	// Blend weight ramps linearly from 0 at threshold to maxFRWeight at 3x threshold.
	rampEnd := fisherRaoAccessThreshold * 3
	blend := float64(accessCount-fisherRaoAccessThreshold) / float64(rampEnd-fisherRaoAccessThreshold)
	if blend > 1.0 {
		blend = 1.0
	}
	frWeight := blend * maxFRWeight

	return clampScore((1-frWeight)*cosine + frWeight*frSim)
}

// frDistanceToSimilarity converts Fisher-Rao distance to a [0,1] similarity
// score using a softmax-style transformation: sim = 2/(1+exp(d)) which maps
// d=0 → 1.0 and d→∞ → 0.0.
func frDistanceToSimilarity(d float64) float64 {
	return 2.0 / (1.0 + math.Exp(d))
}

func clampSigma(s float64) float64 {
	if s < minSigma {
		return minSigma
	}
	return s
}

func clampScore(s float64) float64 {
	if s < 0 {
		return 0
	}
	if s > 1 {
		return 1
	}
	return s
}

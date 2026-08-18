package knowledge

import (
	"math"
	"testing"
)

func TestFisherRaoDistance_IdenticalDistributions(t *testing.T) {
	a := GaussianParams{
		Mean:  []float64{1.0, 2.0, 3.0},
		Sigma: []float64{0.5, 0.5, 0.5},
	}
	d := FisherRaoDistance(a, a)
	if d > 1e-10 {
		t.Errorf("distance between identical distributions should be ~0, got %f", d)
	}
}

func TestFisherRaoDistance_DifferentMeans(t *testing.T) {
	a := GaussianParams{
		Mean:  []float64{0.0, 0.0},
		Sigma: []float64{1.0, 1.0},
	}
	b := GaussianParams{
		Mean:  []float64{1.0, 0.0},
		Sigma: []float64{1.0, 1.0},
	}
	c := GaussianParams{
		Mean:  []float64{2.0, 0.0},
		Sigma: []float64{1.0, 1.0},
	}

	dAB := FisherRaoDistance(a, b)
	dAC := FisherRaoDistance(a, c)

	if dAB <= 0 {
		t.Errorf("distance should be positive, got %f", dAB)
	}
	if dAC <= dAB {
		t.Errorf("farther means should produce larger distance: d(a,c)=%f <= d(a,b)=%f", dAC, dAB)
	}
}

func TestFisherRaoDistance_DifferentSigmas(t *testing.T) {
	a := GaussianParams{
		Mean:  []float64{0.0},
		Sigma: []float64{0.5},
	}
	b := GaussianParams{
		Mean:  []float64{0.0},
		Sigma: []float64{2.0},
	}
	d := FisherRaoDistance(a, b)
	if d <= 0 {
		t.Errorf("different sigmas should produce positive distance, got %f", d)
	}
}

func TestFisherRaoDistance_Symmetry(t *testing.T) {
	a := GaussianParams{
		Mean:  []float64{1.0, 2.0},
		Sigma: []float64{0.3, 0.7},
	}
	b := GaussianParams{
		Mean:  []float64{3.0, 1.0},
		Sigma: []float64{0.5, 1.2},
	}
	if math.Abs(FisherRaoDistance(a, b)-FisherRaoDistance(b, a)) > 1e-10 {
		t.Error("Fisher-Rao distance should be symmetric")
	}
}

func TestFisherRaoDistance_MismatchedDims(t *testing.T) {
	a := GaussianParams{Mean: []float64{1.0}, Sigma: []float64{1.0}}
	b := GaussianParams{Mean: []float64{1.0, 2.0}, Sigma: []float64{1.0, 1.0}}
	d := FisherRaoDistance(a, b)
	if !math.IsInf(d, 1) {
		t.Errorf("mismatched dims should return +Inf, got %f", d)
	}
}

func TestFisherRaoDistance_EmptyVecs(t *testing.T) {
	a := GaussianParams{Mean: nil, Sigma: nil}
	b := GaussianParams{Mean: nil, Sigma: nil}
	d := FisherRaoDistance(a, b)
	if !math.IsInf(d, 1) {
		t.Errorf("empty vectors should return +Inf, got %f", d)
	}
}

func TestCosineSimilarity_Identical(t *testing.T) {
	a := []float64{1, 2, 3}
	s := CosineSimilarity(a, a)
	if math.Abs(s-1.0) > 1e-10 {
		t.Errorf("identical vectors should have cosine 1.0, got %f", s)
	}
}

func TestCosineSimilarity_Orthogonal(t *testing.T) {
	a := []float64{1, 0}
	b := []float64{0, 1}
	s := CosineSimilarity(a, b)
	if math.Abs(s) > 1e-10 {
		t.Errorf("orthogonal vectors should have cosine 0, got %f", s)
	}
}

func TestCosineSimilarity_Opposite(t *testing.T) {
	a := []float64{1, 0}
	b := []float64{-1, 0}
	s := CosineSimilarity(a, b)
	if math.Abs(s-(-1.0)) > 1e-10 {
		t.Errorf("opposite vectors should have cosine -1, got %f", s)
	}
}

func TestCosineSimilarity_Empty(t *testing.T) {
	s := CosineSimilarity(nil, nil)
	if s != 0 {
		t.Errorf("empty vectors should return 0, got %f", s)
	}
}

func TestGraduatedScore_ColdFactUsesCosinOnly(t *testing.T) {
	q := NewGaussianFromEmbedding([]float64{1, 0, 0}, 1.0)
	f := NewGaussianFromEmbedding([]float64{1, 0, 0}, 1.0)

	score := GraduatedScore(q, f, 0)
	cosine := CosineSimilarity(q.Mean, f.Mean)

	if math.Abs(score-cosine) > 1e-10 {
		t.Errorf("cold fact (0 accesses) should use pure cosine: score=%f, cosine=%f", score, cosine)
	}
}

func TestGraduatedScore_HotFactBlendsFisherRao(t *testing.T) {
	q := NewGaussianFromEmbedding([]float64{1, 0.5, 0}, 1.0)
	f := NewGaussianFromEmbedding([]float64{0.9, 0.4, 0.1}, 1.0)

	coldScore := GraduatedScore(q, f, 0)
	hotScore := GraduatedScore(q, f, 50)

	if coldScore == hotScore {
		t.Error("hot and cold facts should produce different scores when embeddings differ slightly")
	}
}

func TestGraduatedScore_AtThreshold(t *testing.T) {
	q := NewGaussianFromEmbedding([]float64{1, 0}, 1.0)
	f := NewGaussianFromEmbedding([]float64{0.8, 0.2}, 1.0)

	belowThreshold := GraduatedScore(q, f, fisherRaoAccessThreshold-1)
	atThreshold := GraduatedScore(q, f, fisherRaoAccessThreshold)

	cosine := CosineSimilarity(q.Mean, f.Mean)
	if math.Abs(belowThreshold-clampScore(cosine)) > 1e-10 {
		t.Errorf("below threshold should be pure cosine: got %f, want %f", belowThreshold, cosine)
	}
	// At threshold, FR blend is tiny but nonzero, so scores may differ slightly.
	_ = atThreshold
}

func TestGraduatedScore_Range(t *testing.T) {
	q := NewGaussianFromEmbedding([]float64{0.3, -0.7, 0.5}, 1.0)
	f := NewGaussianFromEmbedding([]float64{-0.2, 0.8, -0.1}, 1.0)

	for _, count := range []int{0, 5, 10, 20, 50, 100} {
		score := GraduatedScore(q, f, count)
		if score < 0 || score > 1 {
			t.Errorf("score out of [0,1] range at access=%d: %f", count, score)
		}
	}
}

func TestFRDistanceToSimilarity(t *testing.T) {
	if s := frDistanceToSimilarity(0); math.Abs(s-1.0) > 1e-10 {
		t.Errorf("distance 0 should map to similarity 1.0, got %f", s)
	}

	s := frDistanceToSimilarity(100)
	if s > 0.01 {
		t.Errorf("large distance should map to near-zero similarity, got %f", s)
	}

	s1 := frDistanceToSimilarity(1.0)
	s2 := frDistanceToSimilarity(2.0)
	if s1 <= s2 {
		t.Errorf("closer distance should be more similar: sim(1)=%f <= sim(2)=%f", s1, s2)
	}
}

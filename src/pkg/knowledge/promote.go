package knowledge

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"time"
)

const promoteRequestTimeout = 15 * time.Second

// PromoteRequest describes a fact to promote from one wiki layer to another.
type PromoteRequest struct {
	Slug      string    `json:"slug"`
	FromLayer LayerType `json:"from_layer"`
	ToLayer   LayerType `json:"to_layer"`
	Reason    string    `json:"reason"`
	Promoter  string    `json:"promoter"`
}

// PromoteResult captures the outcome of a promotion attempt.
type PromoteResult struct {
	Slug      string    `json:"slug"`
	FromLayer LayerType `json:"from_layer"`
	ToLayer   LayerType `json:"to_layer"`
	Success   bool      `json:"success"`
	Error     string    `json:"error,omitempty"`
}

// Promoter handles fact promotion between wiki layers. Promotion copies a fact
// from a lower-precedence layer to a higher-precedence one (e.g., project → org)
// with provenance metadata.
type Promoter struct {
	layers map[LayerType]*Client
	config CuratorConfig
	logger *slog.Logger
}

// NewPromoter creates a promoter from the configured wiki layers.
func NewPromoter(layers []LayerConfig, config CuratorConfig, logger *slog.Logger) *Promoter {
	clients := make(map[LayerType]*Client)
	for _, l := range layers {
		endpoint := l.Endpoint()
		if endpoint == "" {
			continue
		}
		clients[l.Type] = NewClient(endpoint, logger)
	}

	return &Promoter{
		layers: clients,
		config: config,
		logger: logger,
	}
}

// Promote copies a fact from one layer to another. The fact is read from the
// source layer, enriched with provenance, and ingested into the target layer.
// Promotion only flows upward (project → org → community); attempting to
// promote downward returns an error.
func (p *Promoter) Promote(ctx context.Context, req PromoteRequest) PromoteResult {
	result := PromoteResult{
		Slug:      req.Slug,
		FromLayer: req.FromLayer,
		ToLayer:   req.ToLayer,
	}

	if !isUpwardPromotion(req.FromLayer, req.ToLayer) {
		result.Error = fmt.Sprintf("cannot promote from %s to %s: promotion only flows upward", req.FromLayer, req.ToLayer)
		return result
	}

	sourceClient, ok := p.layers[req.FromLayer]
	if !ok {
		result.Error = fmt.Sprintf("source layer %s has no configured endpoint", req.FromLayer)
		return result
	}

	targetClient, ok := p.layers[req.ToLayer]
	if !ok {
		result.Error = fmt.Sprintf("target layer %s has no configured endpoint", req.ToLayer)
		return result
	}

	page, err := sourceClient.ReadPage(ctx, req.Slug)
	if err != nil {
		result.Error = fmt.Sprintf("reading fact from %s: %v", req.FromLayer, err)
		return result
	}

	fact := ExtractedFact{
		Title:      page.Title,
		Body:       page.Body,
		Type:       FactType(page.Type),
		Confidence: page.Confidence,
		Tags:       page.Tags,
		SourcePR:   fmt.Sprintf("promoted from %s by %s: %s", req.FromLayer, req.Promoter, req.Reason),
		SourceDate: time.Now(),
	}

	if err := p.ingestToLayer(ctx, targetClient, []ExtractedFact{fact}); err != nil {
		result.Error = fmt.Sprintf("ingesting to %s: %v", req.ToLayer, err)
		return result
	}

	result.Success = true
	p.logger.Info("fact promoted",
		"slug", req.Slug,
		"from", req.FromLayer,
		"to", req.ToLayer,
		"promoter", req.Promoter,
	)
	return result
}

// AutoPromoteCandidates scans the source layer for facts with confidence above
// the configured threshold and returns them as promotion candidates. This is
// used by the curator's scheduled auto-promote flow.
func (p *Promoter) AutoPromoteCandidates(ctx context.Context, fromLayer, toLayer LayerType) ([]PromoteRequest, error) {
	client, ok := p.layers[fromLayer]
	if !ok {
		return nil, fmt.Errorf("layer %s has no configured endpoint", fromLayer)
	}

	if !isUpwardPromotion(fromLayer, toLayer) {
		return nil, fmt.Errorf("cannot promote from %s to %s: not upward", fromLayer, toLayer)
	}

	pages, err := client.ListPages(ctx, "")
	if err != nil {
		return nil, fmt.Errorf("listing pages in %s: %w", fromLayer, err)
	}

	threshold := p.config.AutoPromoteThreshold
	var candidates []PromoteRequest
	for _, page := range pages {
		if page.Confidence >= threshold && page.Status == "verified" {
			candidates = append(candidates, PromoteRequest{
				Slug:      page.Slug,
				FromLayer: fromLayer,
				ToLayer:   toLayer,
				Reason:    fmt.Sprintf("auto-promote: confidence %.2f >= threshold %.2f", page.Confidence, threshold),
				Promoter:  "curator",
			})
		}
	}

	p.logger.Info("auto-promote candidates found",
		"from", fromLayer,
		"to", toLayer,
		"candidates", len(candidates),
		"threshold", threshold,
	)
	return candidates, nil
}

// isUpwardPromotion returns true if promoting from→to moves the fact to a
// broader scope (lower precedence number = narrower scope).
// Valid: personal→project, project→org, org→community
func isUpwardPromotion(from, to LayerType) bool {
	return from.Precedence() < to.Precedence()
}

func (p *Promoter) ingestToLayer(ctx context.Context, client *Client, facts []ExtractedFact) error {
	payload, err := json.Marshal(facts)
	if err != nil {
		return fmt.Errorf("marshaling facts: %w", err)
	}

	reqCtx, cancel := context.WithTimeout(ctx, promoteRequestTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, client.baseURL+"/api/ingest", bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("creating ingest request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("ingest request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return fmt.Errorf("ingest returned HTTP %d", resp.StatusCode)
	}

	return nil
}

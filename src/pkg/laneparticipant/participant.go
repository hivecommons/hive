// Package laneparticipant is Hive's optional Crux lane adapter. It translates
// Hive domain events and dispatches only explicitly registered work schemas.
// It has no forge client, prompt executor, agent-control token or write grant.
package laneparticipant

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/hivecommons/hive/crux/api"
	"github.com/hivecommons/hive/crux/lanes"
	"github.com/hivecommons/hive/pkg/governor"
)

const (
	ModeChangeSchema      = "hive-governor-mode-change-v1"
	SnapshotRequestSchema = "hive-governor-snapshot-request-v1"
	SnapshotSchema        = "hive-governor-snapshot-v1"
	DefaultTimeout        = 5 * time.Second
	DefaultPoll           = time.Second
	eventDigestBytes      = 16
)

var ErrUnregistered = errors.New("lane work schema or publisher not registered")

type Handler func(context.Context, api.LaneEnvelope) error
type registration struct {
	projects map[string]bool
	handler  Handler
}

type Participant struct {
	client      *api.Client
	lane, group string
	handlers    map[string]registration
	logger      *slog.Logger
}

func New(client *api.Client, lane, group string, logger *slog.Logger) (*Participant, error) {
	if client == nil || !lanes.ValidID(lane) || !lanes.ValidID(group) {
		return nil, errors.New("incomplete Hive lane participant configuration")
	}
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &Participant{client: client, lane: lane, group: group, handlers: map[string]registration{}, logger: logger}, nil
}

// Register is called before Run. A schema alone never grants another project
// execution permission; each handler separately names its trusted publishers.
func (p *Participant) Register(schema string, projects []string, handler Handler) error {
	if !lanes.ValidID(schema) || len(projects) == 0 || handler == nil {
		return ErrUnregistered
	}
	if _, exists := p.handlers[schema]; exists {
		return errors.New("lane work schema already registered")
	}
	allowed := map[string]bool{}
	for _, project := range projects {
		if !lanes.ValidID(project) {
			return ErrUnregistered
		}
		allowed[project] = true
	}
	p.handlers[schema] = registration{projects: allowed, handler: handler}
	return nil
}

func eventID(prefix string, data []byte) string {
	sum := sha256.Sum256(data)
	return prefix + "-" + hex.EncodeToString(sum[:eventDigestBytes])
}

func (p *Participant) PublishModeChange(ctx context.Context, change governor.ModeChange) error {
	data, err := json.Marshal(change)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, DefaultTimeout)
	defer cancel()
	_, err = p.client.PublishLane(ctx, p.lane, api.LanePublish{
		ID: eventID("mode", data), Kind: api.LaneEvent, Schema: ModeChangeSchema, Payload: data})
	return err
}

// RegisterSnapshot is the built-in read-only work adapter. Requests are strict
// empty objects, never prompts or forge actions. Responses correlate to the
// neutral work ID and are published using this participant's own credential.
func (p *Participant) RegisterSnapshot(projects []string, read func() governor.State) error {
	if read == nil {
		return ErrUnregistered
	}
	return p.Register(SnapshotRequestSchema, projects, func(ctx context.Context, envelope api.LaneEnvelope) error {
		var request map[string]json.RawMessage
		if err := json.Unmarshal(envelope.Payload, &request); err != nil || request == nil || len(request) != 0 {
			return errors.New("snapshot request must be an empty JSON object")
		}
		data, err := json.Marshal(struct {
			RequestID string         `json:"request_id"`
			State     governor.State `json:"state"`
		}{envelope.ID, read()})
		if err != nil {
			return err
		}
		_, err = p.client.PublishLane(ctx, p.lane, api.LanePublish{
			ID:   eventID("snapshot", append([]byte(envelope.Project+"."), data...)),
			Kind: api.LaneEvent, Schema: SnapshotSchema, Payload: data})
		return err
	})
}

// HandleOnce processes at most one lease. Handler execution is bounded by that
// lease; longer-running handlers should use the explicit Crux renewal API in
// their own adapter rather than treating this helper as an execution guarantee.
func (p *Participant) HandleOnce(ctx context.Context) (bool, error) {
	callCtx, cancel := context.WithTimeout(ctx, DefaultTimeout)
	claim, err := p.client.ClaimLane(callCtx, p.lane, p.group)
	cancel()
	if err != nil || claim == nil {
		return false, err
	}
	receipt := api.LaneReceipt{Sequence: claim.Sequence, Receipt: claim.Receipt}
	r, ok := p.handlers[claim.Envelope.Schema]
	if !ok || !r.projects[claim.Envelope.Project] {
		err = ErrUnregistered
	} else {
		workCtx, stop := context.WithDeadline(ctx, claim.Expires)
		err = r.handler(workCtx, claim.Envelope)
		if err == nil {
			err = workCtx.Err()
		}
		stop()
	}
	callCtx, cancel = context.WithTimeout(ctx, DefaultTimeout)
	defer cancel()
	if err != nil {
		return true, errors.Join(err, p.client.NackLane(callCtx, p.lane, p.group, receipt))
	}
	return true, p.client.AckLane(callCtx, p.lane, p.group, receipt)
}

func (p *Participant) Run(ctx context.Context) error {
	ticker := time.NewTicker(DefaultPoll)
	defer ticker.Stop()
	for {
		_, err := p.HandleOnce(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			p.logger.Warn("Hive lane work failed", "error", err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func (p *Participant) ObserveMode(ctx context.Context, change governor.ModeChange) {
	if err := p.PublishModeChange(ctx, change); err != nil {
		p.logger.Warn("Hive lane event publish failed; durable delivery is not confirmed", "error", fmt.Errorf("mode-change: %w", err))
	}
}

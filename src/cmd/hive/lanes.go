package main

import (
	"context"
	"errors"
	"os"
	"strings"

	"github.com/hivecommons/hive/crux/api"
	"github.com/hivecommons/hive/pkg/governor"
	"github.com/hivecommons/hive/pkg/laneparticipant"
)

// HIVE_LANES_ADDRESS is the opt-in gate. No broker or forge permissions are
// inferred from being Hive; its token is an ordinary configured Crux principal.
func (b *boot) installLaneParticipant() {
	address := os.Getenv("HIVE_LANES_ADDRESS")
	if address == "" {
		return
	}
	lane, group, token := os.Getenv("HIVE_LANES_LANE"), os.Getenv("HIVE_LANES_GROUP"), os.Getenv("HIVE_LANES_TOKEN")
	if lane == "" || group == "" || token == "" {
		b.logger.Error("Hive lane participant disabled: incomplete configuration")
		return
	}
	client, err := api.NewClient(address, token)
	if err != nil {
		b.logger.Error("Hive lane participant disabled: invalid Crux address")
		return
	}
	participant, err := laneparticipant.New(client, lane, group, b.logger)
	if err != nil {
		b.logger.Error("Hive lane participant disabled: invalid configuration")
		return
	}
	// Snapshot work is optional and read-only. No configured publisher means
	// no registered handler, not a wildcard grant.
	if publishers := os.Getenv("HIVE_LANES_SNAPSHOT_PROJECTS"); publishers != "" {
		if err := participant.RegisterSnapshot(strings.Split(publishers, ","), b.gov.GetState); err != nil {
			b.logger.Error("Hive lane participant disabled: invalid snapshot publisher configuration")
			return
		}
	}
	ctx := b.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	laneCtx, cancel := context.WithCancel(ctx)
	b.cleanup.push(cancel)
	// Keep the existing hook observer; durable publication is an additional
	// post-commit observer, not a replacement for local hooks.
	b.gov.AddModeChangeObserver(func(change governor.ModeChange) { participant.ObserveMode(laneCtx, change) })
	go func() {
		if err := participant.Run(laneCtx); err != nil && !errors.Is(err, context.Canceled) {
			b.logger.Error("Hive lane participant stopped", "error", err)
		}
	}()
}

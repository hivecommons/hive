package main

import (
	"context"
	"log/slog"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/dashboard"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/questionclose"
	"github.com/hivecommons/hive/pkg/scheduler"
)

// Question auto-close (hivecommons/hive#9584) - composition root.
//
// pkg/questionclose owns the durable schedule and every GitHub decision; the
// scheduler feeds it the classified issue list and puts its answer contract
// in the scanner kick. This file builds it from config and starts its loop.

// questionAutoclosePath is the schedule's on-disk location (the same /data
// volume as the issue-claims ledger; overridable in tests).
var questionAutoclosePath = "/data/question-autoclose.json"

// wireQuestionAutoclose attaches the auto-closer to the scheduler and starts
// its loop, or does nothing when governor.question_autoclose is off. A
// corrupt schedule file is logged and the manager starts empty rather than
// failing boot. client is resolved per tick because the GitHub client is
// rebuilt after boot when App auth completes.
func wireQuestionAutoclose(ctx context.Context, cfg *config.Config, sched *scheduler.Scheduler, client func() *github.Client, logger *slog.Logger) *questionclose.Manager {
	if cfg == nil || sched == nil {
		return nil
	}
	settings := questionclose.SettingsFromConfig(cfg.Governor.QuestionAutoclose)
	if !settings.Enabled {
		if logger != nil {
			logger.Info("question-autoclose: disabled by config")
		}
		return nil
	}
	tracker := func() questionclose.Tracker {
		if client == nil {
			return nil
		}
		gh := client()
		if gh == nil {
			return nil
		}
		return gh
	}
	m, err := questionclose.New(questionAutoclosePath, settings, tracker, logger)
	if err != nil && logger != nil {
		logger.Warn("question-autoclose: could not load persisted schedule, starting empty", "path", questionAutoclosePath, "error", err)
	}
	sched.SetQuestionAutocloser(m)
	go m.Run(ctx, questionclose.DefaultTickInterval)
	if logger != nil {
		logger.Info("question-autoclose: enabled", "window", settings.Window.String(), "labels", settings.QuestionLabels, "human_label", settings.HumanLabel)
	}
	return m
}

// questionAutocloseView adapts the live questionclose.Manager to the
// dashboard's consumer-defined QuestionAutocloseSchedule, so pkg/dashboard
// does not import pkg/questionclose. It reads the manager through *boot at
// call time: the dashboard dependencies are built before bootSupervision
// constructs the manager, and a nil manager (feature off) reads as disabled
// with an empty schedule.
type questionAutocloseView struct{ b *boot }

func (v questionAutocloseView) manager() *questionclose.Manager {
	if v.b == nil {
		return nil
	}
	return v.b.questionAutoclose.Load()
}

func (v questionAutocloseView) Enabled() bool { return v.manager().Enabled() }

func (v questionAutocloseView) ScheduledQuestions() []dashboard.QuestionAutocloseScheduled {
	entries := v.manager().Scheduled()
	out := make([]dashboard.QuestionAutocloseScheduled, 0, len(entries))
	for _, e := range entries {
		out = append(out, dashboard.QuestionAutocloseScheduled{
			Repo:       e.Repo,
			Issue:      e.Issue,
			AnsweredAt: e.AnsweredAt,
			Deadline:   e.Deadline,
		})
	}
	return out
}

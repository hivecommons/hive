package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/customers"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/meetings/betty"
	"github.com/hivecommons/hive/pkg/meetings/meetllm"
)

// Goal and satisfaction inference (hivecommons/hive#18742). It is opt-in
// (customers.inference.enabled), runs on the meetings model route, and only
// ever files suggestions for a person to confirm, edit or reject.

const (
	// customersInferenceInterval is how often the pass looks for a customer
	// to read. Each pass reads at most customers.InferMaxPerPass people.
	customersInferenceInterval = 30 * time.Minute
	// customersInferenceDailyTokens is the day's token budget, shared by
	// every person. The meetings model ledger enforces it per call.
	customersInferenceDailyTokens = 40000
	// customersInferenceCallTimeout is the outer bound on one model call.
	customersInferenceCallTimeout = 30 * time.Second
	// customersInferenceKeepDays is how many days of usage ledgers are kept.
	customersInferenceKeepDays = 7
	customersInferenceUsageDir = "inference"
	customersUsageDayLayout    = "2006-01-02"
	// customersEvidenceBody is how much of an issue body is read.
	customersEvidenceBody = 240
)

// customersEvidence collects, per customer, what they wrote or did that the
// hive already holds: the issues they opened, the pull requests they
// authored, and mail from them that was approved. References only point at
// where each item lives. The text is data written by people and is cleaned,
// scanned and fenced before it reaches a model (customers.SafeEvidence).
func customersEvidence(src customersSources, svc *customers.Service) map[string][]customers.Evidence {
	out := map[string][]customers.Evidence{}
	if src.cfg == nil || svc == nil {
		return out
	}
	store := svc.Store()
	add := func(kind customers.IdentityKind, value string, e customers.Evidence) {
		if p, ok := store.Lookup(kind, value); ok {
			out[p.ID] = append(out[p.ID], e)
		}
	}
	org := src.cfg.Project.Org
	if src.last != nil {
		for _, is := range src.last.Issues.Items {
			if is.Number <= 0 || is.Repo == "" || !is.AuthorIsHuman || !customersIsPerson(src.cfg, is.Author) {
				continue
			}
			body := strings.Join(strings.Fields(is.Body), " ")
			if len(body) > customersEvidenceBody {
				body = body[:customersEvidenceBody]
			}
			add(customers.KindGitHub, is.Author, customers.Evidence{
				Ref:  customers.IssueRef(qualifyClaimRepo(org, is.Repo), is.Number),
				Text: "Issue (" + is.State + "): " + is.Title + ". " + body,
			})
		}
		prs := append(append([]github.PullRequest{}, src.last.PRs.Items...), src.last.PRs.Attributed...)
		for _, pr := range prs {
			if pr.Number <= 0 || pr.Repo == "" || pr.AppAuthored || pr.AuthorNonHuman || !customersIsPerson(src.cfg, pr.Author) {
				continue
			}
			state := pr.State
			if !pr.MergedAt.IsZero() {
				state = "merged"
			}
			add(customers.KindGitHub, pr.Author, customers.Evidence{
				Ref:  customers.PRRef(qualifyClaimRepo(org, pr.Repo), pr.Number),
				Text: "Pull request (" + state + "): " + pr.Title,
			})
		}
	}
	for _, it := range src.mail {
		if it.AwaitingApproval || it.ID == "" {
			continue
		}
		add(customers.KindEmail, customersAddress(it.Sender), customers.Evidence{
			Ref:  customers.InboxRef("email", it.ID),
			Text: "Email: " + it.Title + ". " + it.Excerpt,
		})
	}
	return out
}

// runCustomersInferenceWith reads up to customers.InferMaxPerPass due
// customers through the model. It stops at the first budget error. audit, if
// set, hears of every pass that called the model, with counts and never
// text. It returns how many customers were sent to the model.
func runCustomersInferenceWith(ctx context.Context, svc *customers.Service, evidence map[string][]customers.Evidence, model customers.Completer,
	now time.Time, audit func(personID string, res customers.InferResult, err error), logger *slog.Logger) int {
	ids := make([]string, 0, len(evidence))
	for id := range evidence {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	read := 0
	for _, id := range ids {
		if ctx.Err() != nil || read >= customers.InferMaxPerPass {
			break
		}
		res, err := svc.Infer(ctx, model, id, evidence[id], now)
		if err == nil && res.Skipped != "" {
			continue // nothing to ask: no evidence, or not due
		}
		read++
		if audit != nil {
			audit(id, res, err)
		}
		if err != nil {
			if logger != nil {
				logger.Warn("[customers] inference did not complete", "error", err)
			}
			if errors.Is(err, meetllm.ErrBudget) {
				break // the day's budget is spent; try again tomorrow
			}
		}
	}
	return read
}

// customersInferenceModel is the meetings model, charged to a daily ledger of
// its own. It reports false when the meetings model is not an OpenAI
// compatible endpoint or the Anthropic API: the in-process route inference
// uses. An agent backend is not used here.
func customersInferenceModel(ctx context.Context, cfg *config.Config, dataDir string, now time.Time, logger *slog.Logger) (customers.Completer, bool) {
	llm := cfg.ResolveMeetingsLLM(ctx)
	dir := filepath.Join(dataDir, customersInferenceUsageDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		if logger != nil {
			logger.Warn("[customers] inference usage directory unavailable", "error", err)
		}
		return nil, false
	}
	pruneCustomersUsage(dir, now)
	mc := meetllm.Config{
		Timeout:     customersInferenceCallTimeout,
		UsageFile:   filepath.Join(dir, "usage-"+now.UTC().Format(customersUsageDayLayout)+".jsonl"),
		MaxTokens:   customersInferenceDailyTokens,
		MaxInFlight: 1,
	}
	switch llm.Backend {
	case config.MeetingsLLMModeOpenAI:
		client, err := meetllm.HTTPClient(llm.CABundle)
		if err != nil {
			return nil, false
		}
		mc.Endpoint, mc.Model, mc.APIKey = llm.Endpoint, llm.Model, llm.APIKey
		return betty.MeetLLM{Client: client, Config: mc}, true
	case config.MeetingsLLMModeAnthropic:
		mc.Provider, mc.Model, mc.APIKey = meetllm.ProviderAnthropic, llm.Model, llm.AnthropicKey
		return betty.MeetLLM{Client: &http.Client{}, Config: mc}, true
	}
	return nil, false
}

// pruneCustomersUsage removes usage ledgers older than the keep window.
func pruneCustomersUsage(dir string, now time.Time) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	cutoff := now.UTC().AddDate(0, 0, -customersInferenceKeepDays)
	for _, e := range entries {
		name := strings.TrimSuffix(strings.TrimPrefix(e.Name(), "usage-"), ".jsonl")
		if day, err := time.Parse(customersUsageDayLayout, name); err == nil && day.Before(cutoff) {
			_ = os.Remove(filepath.Join(dir, e.Name()))
		}
	}
}

// runCustomersInference is one inference pass, when inference is on.
func (b *boot) runCustomersInference(ctx context.Context, cfg *config.Config) {
	if b.customers == nil || cfg == nil || !cfg.CustomersInferenceEnabled() {
		return
	}
	now := time.Now()
	model, ok := customersInferenceModel(ctx, cfg, customersDataDir, now, b.logger)
	if !ok {
		return
	}
	evidence := customersEvidence(b.customersSources(ctx, cfg), b.customers.svc)
	audit := func(id string, res customers.InferResult, err error) {
		if b.dashSrv == nil {
			return
		}
		detail := "person=" + id + ", goals=" + strconv.Itoa(res.Goals) + ", satisfaction=" + strconv.FormatBool(res.Satisfaction)
		if err != nil {
			detail += ", failed=true"
		}
		b.dashSrv.AuditLog("system", "customer_inference", detail, "")
	}
	runCustomersInferenceWith(ctx, b.customers.svc, evidence, model, now, audit, b.logger)
}

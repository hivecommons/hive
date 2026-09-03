package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/hivecommons/hive/pkg/beads"
	"github.com/hivecommons/hive/pkg/planning"
)

// cmdDecompose is the MANUAL Phase-1 entry point for planning intelligence.
// It decomposes an existing epic bead into a DAG of child task beads.
//
// Phase 1 does NOT launch a live agent: the planner's task breakdown is read
// from a file (--plan) or stdin. This keeps the trigger fully manual and the
// planning package free of any agent/tmux dependency. Phase 2/3 will wire the
// architect lane's live output in place of the file/stdin source.
//
// Usage:
//
//	bd decompose <epic-id> --plan plan.txt [--actor <lane>]
//	cat plan.txt | bd decompose <epic-id>
func cmdDecompose(args []string) {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "bd decompose: requires an epic bead ID")
		os.Exit(1)
	}

	epicID := args[0]
	fs := flag.NewFlagSet("decompose", flag.ExitOnError)
	planFile := fs.String("plan", "", "File containing the planner's task list (default: stdin)")
	actor := fs.String("actor", "", "Override child bead actor/lane (default: classifier/architect)")
	autoApprove := fs.Bool("auto-approve", false, "Approve the plan immediately (children claimable now, no review gate)")
	printPrompt := fs.Bool("print-prompt", false, "Print the architect decomposition prompt for this epic and exit")
	_ = fs.Parse(args[1:])

	store := openStore()
	epic, err := store.Get(epicID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "bd decompose: %v\n", err)
		os.Exit(1)
	}

	if *printPrompt {
		fmt.Print(decomposePromptFor(epic))
		return
	}

	output, err := readPlan(*planFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "bd decompose: reading plan: %v\n", err)
		os.Exit(1)
	}

	// Phase 1 planner is a passthrough of the file/stdin content; the prompt is
	// still built (and could be printed for the operator) but no agent runs.
	planner := func(_ context.Context, _ string) (string, error) { return output, nil }

	result, err := planning.Decompose(context.Background(), store, epic, planner, planning.Options{Actor: *actor, AutoApprove: *autoApprove})
	if err != nil {
		fmt.Fprintf(os.Stderr, "bd decompose: %v\n", err)
		os.Exit(1)
	}

	planStatus := planning.PlanStatusDraft
	if *autoApprove {
		planStatus = planning.PlanStatusApproved
	}
	fmt.Printf("Decomposed epic %s into %d child bead(s):\n", epic.ID, len(result.Children))
	for i, child := range result.Children {
		fmt.Printf("  %s  [%s]  %s\n", child.ID, child.Meta(planning.MetaExecution), result.Tasks[i].Title)
	}
	fmt.Printf("Epic %s marked plan_status=%s\n", epic.ID, planStatus)
}

// readPlan returns the plan text from path, or from stdin when path is empty.
func readPlan(path string) (string, error) {
	if path == "" {
		data, err := io.ReadAll(os.Stdin)
		if err != nil {
			return "", err
		}
		return string(data), nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// decomposePromptFor is exposed so an operator can preview the architect prompt
// that the planner should be given (e.g. to paste into a live agent). It is a
// thin wrapper over planning.BuildPrompt.
func decomposePromptFor(epic *beads.Bead) string {
	return planning.BuildPrompt(epic)
}

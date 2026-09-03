// bd is a CLI wrapper around the Hive v2 beads store.
// It provides the same command interface that agents reference in their
// loop prompts (bd list, bd ready, bd create, bd update, bd close, etc.)
// while delegating all persistence to the pkg/beads Store.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/hivecommons/hive/pkg/beads"
)

// agentDirPattern matches /home/dev/<agent>-beads or /data/beads/<agent>.
var agentDirPattern = regexp.MustCompile(`^(/home/dev/[^/]+-beads|/data/beads/[^/]+)$`)

// agentWorkdirPattern matches /data/agents/<name> and extracts the agent name.
var agentWorkdirPattern = regexp.MustCompile(`^/data/agents/([^/]+)$`)

func resolveDir() string {
	// 1. BD_DIR env var — explicit override.
	if dir := os.Getenv("BD_DIR"); dir != "" {
		return dir
	}

	cwd, err := os.Getwd()
	if err != nil {
		return "."
	}

	// 2. If cwd matches the beads dir convention, use it directly.
	if agentDirPattern.MatchString(cwd) {
		if resolved, err := filepath.EvalSymlinks(cwd); err == nil {
			return resolved
		}
		return cwd
	}

	// 3. If cwd is /data/agents/<name>, derive /data/beads/<name>.
	if m := agentWorkdirPattern.FindStringSubmatch(cwd); m != nil {
		return filepath.Join("/data/beads", m[1])
	}

	// 4. Fall back to cwd.
	return cwd
}

func main() {
	if len(os.Args) < 2 {
		printUsage()
		os.Exit(1)
	}

	cmd := os.Args[1]
	switch cmd {
	case "list":
		cmdList(os.Args[2:])
	case "ready":
		cmdReady(os.Args[2:])
	case "create":
		cmdCreate(os.Args[2:])
	case "update":
		cmdUpdate(os.Args[2:])
	case "close":
		cmdClose(os.Args[2:])
	case "decompose":
		cmdDecompose(os.Args[2:])
	case "reset":
		cmdReset(os.Args[2:])
	case "remember":
		cmdRemember(os.Args[2:])
	case "kb":
		cmdKB(os.Args[2:])
	case "dolt":
		cmdDolt(os.Args[2:])
	case "init":
		cmdInit()
	case "help", "--help", "-h":
		printUsage()
	default:
		fmt.Fprintf(os.Stderr, "bd: unknown command %q\n", cmd)
		printUsage()
		os.Exit(1)
	}
}

func printUsage() {
	fmt.Fprintln(os.Stderr, `Usage: bd <command> [options]

Commands:
  list   [--json] [--status=<s>] [--actor=<a>]  List beads
  ready  [--json] [--actor=<a>]                List open/unblocked beads
  create --title "..." --type <type> --priority <0-4> --actor <name> [--external-ref <ref>]
  update <id> --claim                      Claim a bead (set in_progress)
  update <id> --status <status>            Change status
  update <id> --set-metadata key=value     Set metadata key
  update <id> --unset-metadata key         Remove metadata key
  kb       search "query"                  Search knowledge base
  kb       read <slug>                    Read full fact body
  kb       import-url <url> [--name <n>]  Import document from URL
  kb       import-file <path> [--name <n>] Import document from file
  kb       list-docs                      List imported documents
  remember "<fact>"                        Quick-add an advisory bead
  close  <id>                              Close a bead
  decompose <epic-id> [--plan <file>] [--actor <lane>]
                                           Decompose an epic into a DAG of child task beads
                                           (Phase 1: reads the planner's task list from --plan or stdin)
  reset  [--reason "..."]                  Close all open/in_progress/blocked beads
  dolt   push                              No-op (data already on disk)
  init                                     No-op (store auto-creates)

Environment:
  BD_DIR   Override beads directory (default: cwd or agent convention)`)
}

// openStore creates a Store rooted at the resolved directory.
func openStore() *beads.Store {
	dir := resolveDir()
	store, err := beads.NewStore(dir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "bd: failed to open store at %s: %v\n", dir, err)
		os.Exit(1)
	}
	return store
}

// ---------- list ----------

func cmdList(args []string) {
	fs := flag.NewFlagSet("list", flag.ExitOnError)
	jsonOut := fs.Bool("json", false, "Output as JSON")
	statusFilter := fs.String("status", "", "Filter by status")
	actorFilter := fs.String("actor", "", "Filter by actor name")
	_ = fs.Parse(args)

	store := openStore()
	filter := beads.ListFilter{}
	if *statusFilter != "" {
		s := beads.Status(*statusFilter)
		filter.Status = &s
	}
	if *actorFilter != "" {
		filter.Actor = actorFilter
	}

	results := store.List(filter)
	if *jsonOut {
		printJSON(ensureSlice(results))
	} else {
		printTable(results)
	}
}

// ---------- ready ----------

func cmdReady(args []string) {
	fs := flag.NewFlagSet("ready", flag.ExitOnError)
	jsonOut := fs.Bool("json", false, "Output as JSON")
	actor := fs.String("actor", "", "Filter by actor name")
	_ = fs.Parse(args)

	store := openStore()
	results := store.Ready(*actor)
	if *jsonOut {
		printJSON(ensureSlice(results))
	} else {
		printTable(results)
	}
}

// ---------- create ----------

func cmdCreate(args []string) {
	fs := flag.NewFlagSet("create", flag.ExitOnError)
	title := fs.String("title", "", "Bead title (required)")
	beadType := fs.String("type", "task", "Bead type: bug|feature|task|epic|chore|decision|advisory")
	priority := fs.Int("priority", 2, "Priority 0-4 (0=critical, 4=minor)")
	actor := fs.String("actor", "", "Actor name (required)")
	extRef := fs.String("external-ref", "", "External reference (e.g. issue URL)")
	_ = fs.Parse(args)

	if *title == "" || *actor == "" {
		fmt.Fprintln(os.Stderr, "bd create: --title and --actor are required")
		os.Exit(1)
	}

	const maxPriority = 4
	if *priority < 0 || *priority > maxPriority {
		fmt.Fprintln(os.Stderr, "bd create: --priority must be 0-4")
		os.Exit(1)
	}

	store := openStore()
	// ADVISORY beads upsert rather than create. An advisory agent re-files a
	// finding every cycle for as long as its condition holds, and each re-file
	// used to become a separate bead — which is both how the digest filled with
	// restatements of one problem and why a finding could never be told apart
	// from a stale one. Upsert refreshes the existing bead's last-seen stamp
	// instead, which is the signal the staleness prune consumes. Every other
	// bead type keeps plain Create: two tasks with similar titles are two tasks.
	var (
		b   *beads.Bead
		err error
	)
	if beads.BeadType(*beadType) == beads.TypeAdvisory {
		b, err = store.Upsert(*title, beads.TypeAdvisory, beads.Priority(*priority), *actor, *extRef)
	} else {
		b, err = store.Create(*title, beads.BeadType(*beadType), beads.Priority(*priority), *actor, *extRef)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "bd create: %v\n", err)
		os.Exit(1)
	}

	printJSON(b)
}

// ---------- update ----------

func cmdUpdate(args []string) {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "bd update: requires a bead ID")
		os.Exit(1)
	}

	id := args[0]
	fs := flag.NewFlagSet("update", flag.ExitOnError)
	claim := fs.Bool("claim", false, "Set status to in_progress")
	status := fs.String("status", "", "Set status")
	setMeta := fs.String("set-metadata", "", "Set metadata key=value")
	unsetMeta := fs.String("unset-metadata", "", "Remove metadata key")
	_ = fs.Parse(args[1:])

	store := openStore()

	if *claim {
		if err := store.Claim(id); err != nil {
			fmt.Fprintf(os.Stderr, "bd update: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("Claimed bead %s (status → in_progress)\n", id)
		return
	}

	if *status != "" {
		validStatuses := map[string]bool{
			"open": true, "in_progress": true, "blocked": true, "done": true, "closed": true,
		}
		if !validStatuses[*status] {
			fmt.Fprintf(os.Stderr, "bd update: invalid status %q (valid: open, in_progress, blocked, done, closed)\n", *status)
			os.Exit(1)
		}
		if err := store.Update(id, func(b *beads.Bead) {
			b.Status = beads.Status(*status)
		}); err != nil {
			fmt.Fprintf(os.Stderr, "bd update: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("Updated bead %s status → %s\n", id, *status)
		return
	}

	if *setMeta != "" {
		parts := strings.SplitN(*setMeta, "=", 2)
		const kvPairLen = 2
		if len(parts) != kvPairLen {
			fmt.Fprintln(os.Stderr, "bd update: --set-metadata requires key=value format")
			os.Exit(1)
		}
		if err := store.SetMetadata(id, parts[0], parts[1]); err != nil {
			fmt.Fprintf(os.Stderr, "bd update: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("Set metadata %s=%s on bead %s\n", parts[0], parts[1], id)
		return
	}

	if *unsetMeta != "" {
		if err := store.UnsetMetadata(id, *unsetMeta); err != nil {
			fmt.Fprintf(os.Stderr, "bd update: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("Removed metadata %s from bead %s\n", *unsetMeta, id)
		return
	}

	fmt.Fprintln(os.Stderr, "bd update: specify --claim, --status, --set-metadata, or --unset-metadata")
	os.Exit(1)
}

// ---------- close ----------

func cmdClose(args []string) {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "bd close: requires a bead ID")
		os.Exit(1)
	}

	store := openStore()
	id := args[0]
	if err := store.Close(id); err != nil {
		fmt.Fprintf(os.Stderr, "bd close: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("Closed bead %s\n", id)
}

// ---------- reset ----------

func cmdReset(args []string) {
	fs := flag.NewFlagSet("reset", flag.ExitOnError)
	reason := fs.String("reason", "manual reset", "Reason for closing all beads")
	_ = fs.Parse(args)

	store := openStore()
	closed, err := store.CloseAll(*reason)
	if err != nil {
		fmt.Fprintf(os.Stderr, "bd reset: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("Closed %d bead(s) (reason: %s)\n", closed, *reason)
}

// ---------- dolt push (no-op) ----------

func cmdRemember(args []string) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "bd remember: requires a fact string")
		os.Exit(1)
	}
	fact := strings.Join(args, " ")
	store := openStore()
	b, err := store.Create(fact, beads.TypeAdvisory, beads.Priority(4), "system", "")
	if err != nil {
		fmt.Fprintf(os.Stderr, "bd remember: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("Remembered: %s (bead %s)\n", fact, b.ID)
}

func cmdDolt(args []string) {
	if len(args) > 0 && args[0] == "push" {
		fmt.Println("bd: data persisted to disk (dolt push is a no-op in Hive v2)")
		return
	}
	fmt.Fprintln(os.Stderr, "bd dolt: only 'push' subcommand is supported")
	os.Exit(1)
}

// ---------- init (no-op) ----------

func cmdInit() {
	// Opening the store auto-creates the directory and file.
	_ = openStore()
	fmt.Println("bd: store initialized (auto-created on first access in Hive v2)")
}

// ensureSlice returns an empty slice instead of nil so JSON output is [] not null.
func ensureSlice(items []*beads.Bead) []*beads.Bead {
	if items == nil {
		return []*beads.Bead{}
	}
	return items
}

// ---------- output helpers ----------

func printJSON(v interface{}) {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		fmt.Fprintf(os.Stderr, "bd: json encode error: %v\n", err)
		os.Exit(1)
	}
}

const (
	idColWidth       = 14
	statusColWidth   = 14
	priorityColWidth = 4
	actorColWidth    = 12
	titleColWidth    = 50
)

func printTable(items []*beads.Bead) {
	if len(items) == 0 {
		fmt.Println("No beads found.")
		return
	}

	fmt.Printf("%-*s %-*s %-*s %-*s %s\n",
		idColWidth, "ID",
		statusColWidth, "STATUS",
		priorityColWidth, "PRI",
		actorColWidth, "ACTOR",
		"TITLE")
	fmt.Println(strings.Repeat("-", idColWidth+statusColWidth+priorityColWidth+actorColWidth+titleColWidth))

	for _, b := range items {
		title := b.Title
		runes := []rune(title)
		if len(runes) > titleColWidth {
			title = string(runes[:titleColWidth-3]) + "..."
		}
		fmt.Printf("%-*s %-*s %-*s %-*s %s\n",
			idColWidth, b.ID,
			statusColWidth, string(b.Status),
			priorityColWidth, strconv.Itoa(int(b.Priority)),
			actorColWidth, b.Actor,
			title)
	}

	fmt.Printf("\n%d bead(s)\n", len(items))
}

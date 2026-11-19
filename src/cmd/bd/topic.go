package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/hivecommons/hive/pkg/beads"
)

// bd topic: durable one-PR-per-topic claims (hivecommons/hive#18493).
//
//	bd topic claim <key> --repo owner/name --pr N [--branch B] [--actor A]
//	bd topic get <key> [--json]
//	bd topic list [--json]
//	bd topic release <key>
//
// Exit codes: 0 success, 1 usage or store error, 2 refused (the topic is
// held by a different PR) or not found on get.
const (
	topicExitOK      = 0
	topicExitError   = 1
	topicExitRefused = 2
)

const topicUsage = `Usage:
  bd topic claim <key> --repo owner/name --pr N [--branch B] [--actor A]
  bd topic get <key> [--json]
  bd topic list [--json]
  bd topic release <key>`

func cmdTopic(args []string) {
	if code := runTopic(args, openStore, os.Stdout, os.Stderr); code != topicExitOK {
		os.Exit(code)
	}
}

// runTopic is cmdTopic without the process exit, so every path is testable
// in-process.
func runTopic(args []string, open func() *beads.Store, stdout, stderr io.Writer) int {
	if len(args) < 1 {
		fmt.Fprintln(stderr, topicUsage)
		return topicExitError
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "list":
		return topicList(rest, open, stdout, stderr)
	case "claim", "get", "release":
	default:
		fmt.Fprintf(stderr, "bd topic: unknown subcommand %q\n%s\n", sub, topicUsage)
		return topicExitError
	}
	if len(rest) < 1 || rest[0] == "" || rest[0][0] == '-' {
		fmt.Fprintf(stderr, "bd topic %s: requires a topic key\n", sub)
		return topicExitError
	}
	key, flags := rest[0], rest[1:]
	switch sub {
	case "claim":
		return topicClaim(key, flags, open, stdout, stderr)
	case "get":
		return topicGet(key, flags, open, stdout, stderr)
	default:
		return topicRelease(key, open, stdout, stderr)
	}
}

func topicClaim(key string, args []string, open func() *beads.Store, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("topic claim", flag.ContinueOnError)
	fs.SetOutput(stderr)
	repo := fs.String("repo", "", "Repository as owner/name (required)")
	pr := fs.Int("pr", 0, "Pull request number (required)")
	branch := fs.String("branch", "", "Current head branch")
	actor := fs.String("actor", os.Getenv("HIVE_AGENT"), "Claiming agent (default $HIVE_AGENT)")
	if err := fs.Parse(args); err != nil {
		return topicExitError
	}
	claim, err := open().ClaimTopic(*actor, beads.TopicClaim{Key: key, Repo: *repo, PR: *pr, Branch: *branch})
	if errors.Is(err, beads.ErrTopicClaimed) {
		fmt.Fprintf(stderr, "bd topic claim: refused: %v (work on that PR, or release the topic first)\n", err)
		return topicExitRefused
	}
	if err != nil {
		fmt.Fprintf(stderr, "bd topic claim: %v\n", err)
		return topicExitError
	}
	return writeTopicJSON(stdout, stderr, claim)
}

func topicGet(key string, args []string, open func() *beads.Store, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("topic get", flag.ContinueOnError)
	fs.SetOutput(stderr)
	jsonOut := fs.Bool("json", false, "Output as JSON")
	if err := fs.Parse(args); err != nil {
		return topicExitError
	}
	claim, ok := open().Topic(key)
	if !ok {
		fmt.Fprintf(stderr, "bd topic get: no open claim for %q\n", key)
		return topicExitRefused
	}
	if *jsonOut {
		return writeTopicJSON(stdout, stderr, claim)
	}
	fmt.Fprintln(stdout, formatTopicLine(claim))
	return topicExitOK
}

func topicList(args []string, open func() *beads.Store, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("topic list", flag.ContinueOnError)
	fs.SetOutput(stderr)
	jsonOut := fs.Bool("json", false, "Output as JSON")
	if err := fs.Parse(args); err != nil {
		return topicExitError
	}
	claims := open().OpenTopics()
	if *jsonOut {
		if claims == nil {
			claims = []beads.TopicClaim{}
		}
		return writeTopicJSON(stdout, stderr, claims)
	}
	if len(claims) == 0 {
		fmt.Fprintln(stdout, "No open topic claims.")
		return topicExitOK
	}
	for _, c := range claims {
		fmt.Fprintln(stdout, formatTopicLine(c))
	}
	return topicExitOK
}

func topicRelease(key string, open func() *beads.Store, stdout, stderr io.Writer) int {
	released, err := open().ReleaseTopic(key, beads.TopicStateReleased)
	if err != nil {
		fmt.Fprintf(stderr, "bd topic release: %v\n", err)
		return topicExitError
	}
	if !released {
		fmt.Fprintf(stdout, "No open claim for %q\n", key)
		return topicExitOK
	}
	fmt.Fprintf(stdout, "Released topic %q\n", key)
	return topicExitOK
}

func formatTopicLine(c beads.TopicClaim) string {
	line := fmt.Sprintf("%s → %s#%d", c.Key, c.Repo, c.PR)
	if c.Branch != "" {
		line += " (branch " + c.Branch + ")"
	}
	return line
}

func writeTopicJSON(stdout, stderr io.Writer, v interface{}) int {
	enc := json.NewEncoder(stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		fmt.Fprintf(stderr, "bd topic: json encode error: %v\n", err)
		return topicExitError
	}
	return topicExitOK
}

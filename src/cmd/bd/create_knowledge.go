package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
)

// knowledgeCreateType is the `bd create --type` value that creates a
// knowledge fact instead of a bead (#17056). Facts live in the knowledge
// store the curator, the TOC and `hivectl knowledge state` work on, so this
// goes to the dashboard's POST /api/knowledge/create, the same way `bd kb`
// does, never into the local bead store.
const knowledgeCreateType = "knowledge"

// maxKnowledgeBodyBytes bounds --body-file; a fact is a note, not a document
// (use `bd kb import-file` for those).
const maxKnowledgeBodyBytes = 256 * 1024

// knowledgeCreateRequest is the body of POST /api/knowledge/create. Empty
// fields are omitted so the dashboard applies its defaults (layer project,
// type pattern, state approved).
type knowledgeCreateRequest struct {
	Title string   `json:"title"`
	Body  string   `json:"body"`
	Type  string   `json:"type,omitempty"`
	Tags  []string `json:"tags,omitempty"`
	Layer string   `json:"layer,omitempty"`
	State string   `json:"state,omitempty"`
}

// stringList is a repeatable string flag.
type stringList []string

func (s *stringList) String() string { return strings.Join(*s, ",") }

func (s *stringList) Set(v string) error {
	*s = append(*s, v)
	return nil
}

// createTypeArg returns the value of the last --type/-type flag in args
// (Go's flag package keeps the last one), or "" when there is none.
func createTypeArg(args []string) string {
	typ := ""
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			break
		}
		name := strings.TrimLeft(a, "-")
		if name == a {
			continue
		}
		if v, ok := strings.CutPrefix(name, "type="); ok {
			typ = v
			continue
		}
		if name == "type" && i+1 < len(args) {
			typ = args[i+1]
			i++
		}
	}
	return typ
}

// parseKnowledgeCreate turns `bd create --type knowledge …` arguments into
// the request body. stdin is read for --body-file -.
func parseKnowledgeCreate(args []string, stdin io.Reader) (knowledgeCreateRequest, error) {
	fs := flag.NewFlagSet("create", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.String("type", knowledgeCreateType, "must be knowledge")
	title := fs.String("title", "", "Fact title (required)")
	body := fs.String("body", "", "Fact body")
	bodyFile := fs.String("body-file", "", "Read the fact body from a file (- for stdin)")
	state := fs.String("state", "", "Lifecycle state to create the fact in: draft or approved (default approved)")
	layer := fs.String("layer", "", "Knowledge layer, or for a draft the local channel to write to (default project)")
	factType := fs.String("fact-type", "", "Fact type, e.g. pattern, gotcha, decision (default pattern)")
	var tags stringList
	fs.Var(&tags, "tag", "Tag to attach (repeatable)")
	if err := fs.Parse(args); err != nil {
		return knowledgeCreateRequest{}, err
	}
	if fs.NArg() > 0 {
		return knowledgeCreateRequest{}, fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	if strings.TrimSpace(*title) == "" {
		return knowledgeCreateRequest{}, errors.New("--title is required")
	}
	if (*body == "") == (*bodyFile == "") {
		return knowledgeCreateRequest{}, errors.New("exactly one of --body and --body-file is required")
	}
	text := *body
	if *bodyFile != "" {
		var r io.Reader = stdin
		if *bodyFile != "-" {
			f, err := os.Open(*bodyFile)
			if err != nil {
				return knowledgeCreateRequest{}, err
			}
			defer func() { _ = f.Close() }()
			r = f
		}
		data, err := io.ReadAll(io.LimitReader(r, maxKnowledgeBodyBytes+1))
		if err != nil {
			return knowledgeCreateRequest{}, fmt.Errorf("reading --body-file: %w", err)
		}
		if len(data) > maxKnowledgeBodyBytes {
			return knowledgeCreateRequest{}, fmt.Errorf("--body-file is over %d bytes", maxKnowledgeBodyBytes)
		}
		text = string(data)
	}
	if strings.TrimSpace(text) == "" {
		return knowledgeCreateRequest{}, errors.New("the fact body is empty")
	}
	return knowledgeCreateRequest{
		Title: *title,
		Body:  text,
		Type:  *factType,
		Tags:  tags,
		Layer: *layer,
		State: strings.ToLower(strings.TrimSpace(*state)),
	}, nil
}

func cmdCreateKnowledge(args []string) {
	req, err := parseKnowledgeCreate(args, os.Stdin)
	if err != nil {
		fmt.Fprintf(os.Stderr, "bd create --type knowledge: %v\n", err)
		os.Exit(1)
	}
	payload, err := json.Marshal(req)
	if err != nil {
		fmt.Fprintf(os.Stderr, "bd create --type knowledge: %v\n", err)
		os.Exit(1)
	}
	body, err := kbPost(dashboardURL()+"/api/knowledge/create", string(payload))
	if err != nil {
		fmt.Fprintf(os.Stderr, "bd create --type knowledge: %v\n", err)
		os.Exit(1)
	}
	var resp struct {
		Title string `json:"title"`
		Layer string `json:"layer"`
		State string `json:"state"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		fmt.Fprintf(os.Stderr, "bd create --type knowledge: invalid response: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("Created knowledge fact %q in %s (state: %s)\n", resp.Title, resp.Layer, resp.State)
}

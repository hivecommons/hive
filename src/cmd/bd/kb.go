package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

const (
	defaultDashboardURL = "http://localhost:3001"
	kbMaxResponseBytes  = 10 * 1024 * 1024 // 10MB
)

func dashboardURL() string {
	if u := os.Getenv("BD_DASHBOARD_URL"); u != "" {
		return strings.TrimRight(u, "/")
	}
	return defaultDashboardURL
}

func cmdKB(args []string) {
	if len(args) < 1 {
		printKBUsage()
		os.Exit(1)
	}

	sub := args[0]
	switch sub {
	case "search":
		cmdKBSearch(args[1:])
	case "read":
		cmdKBRead(args[1:])
	case "import-url":
		cmdKBImportURL(args[1:])
	case "import-file":
		cmdKBImportFile(args[1:])
	case "import-ctx7":
		cmdKBImportCtx7(args[1:])
	case "ctx7-search":
		cmdKBCtx7Search(args[1:])
	case "list-docs":
		cmdKBListDocs()
	case "help", "--help", "-h":
		printKBUsage()
	default:
		fmt.Fprintf(os.Stderr, "bd kb: unknown subcommand %q\n", sub)
		printKBUsage()
		os.Exit(1)
	}
}

func printKBUsage() {
	fmt.Fprintln(os.Stderr, `Usage: bd kb <subcommand> [options]

Subcommands:
  search "query"                  Search knowledge, print top results
  read <slug>                     Print full fact body
  import-url <url> [--name <n>]   Import document from URL
  import-file <path> [--name <n>] Import document from local file
  import-ctx7 <library-id>        Import library docs from Context7
  ctx7-search <name>              Search Context7 for available libraries
  list-docs                       List imported documents

Environment:
  BD_DASHBOARD_URL   Dashboard API base URL (default: http://localhost:3001)`)
}

func cmdKBSearch(args []string) {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "bd kb search: query required")
		os.Exit(1)
	}
	query := strings.Join(args, " ")

	base := dashboardURL()
	u := fmt.Sprintf("%s/api/knowledge/search?q=%s&limit=10", base, url.QueryEscape(query))

	body, err := kbGet(u)
	if err != nil {
		fmt.Fprintf(os.Stderr, "bd kb search: %v\n", err)
		os.Exit(1)
	}

	var facts []map[string]interface{}
	if err := json.Unmarshal(body, &facts); err != nil {
		fmt.Fprintf(os.Stderr, "bd kb search: invalid response: %v\n", err)
		os.Exit(1)
	}

	if len(facts) == 0 {
		fmt.Println("No results found.")
		return
	}

	for _, f := range facts {
		slug, _ := f["slug"].(string)
		title, _ := f["title"].(string)
		confidence, _ := f["confidence"].(float64)
		factType, _ := f["type"].(string)
		bodyText, _ := f["body"].(string)

		fmt.Printf("## %s\n", title)
		fmt.Printf("   slug: %s | type: %s | confidence: %.0f%%\n", slug, factType, confidence*100)
		preview := bodyText
		const maxPreviewChars = 200
		if len(preview) > maxPreviewChars {
			preview = preview[:maxPreviewChars] + "..."
		}
		if preview != "" {
			fmt.Printf("   %s\n", strings.ReplaceAll(preview, "\n", "\n   "))
		}
		fmt.Println()
	}
}

func cmdKBRead(args []string) {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "bd kb read: slug required")
		os.Exit(1)
	}
	slug := args[0]

	base := dashboardURL()
	body, err := kbGet(fmt.Sprintf("%s/api/knowledge/search?q=%s&limit=50", base, url.QueryEscape(slug)))
	if err != nil {
		fmt.Fprintf(os.Stderr, "bd kb read: %v\n", err)
		os.Exit(1)
	}

	var facts []map[string]interface{}
	if err := json.Unmarshal(body, &facts); err != nil {
		fmt.Fprintf(os.Stderr, "bd kb read: invalid response: %v\n", err)
		os.Exit(1)
	}

	for _, f := range facts {
		if s, _ := f["slug"].(string); s == slug {
			title, _ := f["title"].(string)
			bodyText, _ := f["body"].(string)
			fmt.Printf("# %s\n\n%s\n", title, bodyText)
			return
		}
	}

	fmt.Fprintf(os.Stderr, "bd kb read: fact %q not found\n", slug)
	os.Exit(1)
}

func cmdKBImportURL(args []string) {
	fs := flag.NewFlagSet("import-url", flag.ExitOnError)
	name := fs.String("name", "", "document name (defaults to URL slug)")
	layer := fs.String("layer", "project", "knowledge layer")
	_ = fs.Parse(args) // ExitOnError: Parse never returns on failure, it os.Exits internally

	if fs.NArg() < 1 {
		fmt.Fprintln(os.Stderr, "bd kb import-url: URL required")
		os.Exit(1)
	}
	docURL := fs.Arg(0)

	docName := *name
	if docName == "" {
		docName = urlToName(docURL)
	}

	payload := fmt.Sprintf(`{"name":%q,"url":%q,"layer":%q}`, docName, docURL, *layer)
	body, err := kbPost(dashboardURL()+"/api/knowledge/documents", payload)
	if err != nil {
		fmt.Fprintf(os.Stderr, "bd kb import-url: %v\n", err)
		os.Exit(1)
	}

	var meta map[string]interface{}
	if err := json.Unmarshal(body, &meta); err != nil {
		fmt.Fprintf(os.Stderr, "bd kb import-url: invalid response: %v\n", err)
		os.Exit(1)
	}

	factCount, _ := meta["fact_count"].(float64)
	slug, _ := meta["slug"].(string)
	fmt.Printf("Imported %q → %d facts (slug: %s)\n", docName, int(factCount), slug)
}

func cmdKBImportFile(args []string) {
	fs := flag.NewFlagSet("import-file", flag.ExitOnError)
	name := fs.String("name", "", "document name (defaults to filename)")
	layer := fs.String("layer", "project", "knowledge layer")
	_ = fs.Parse(args) // ExitOnError: Parse never returns on failure, it os.Exits internally

	if fs.NArg() < 1 {
		fmt.Fprintln(os.Stderr, "bd kb import-file: file path required")
		os.Exit(1)
	}
	filePath := fs.Arg(0)

	docName := *name
	if docName == "" {
		docName = strings.TrimSuffix(filepath.Base(filePath), filepath.Ext(filePath))
	}

	payload := fmt.Sprintf(`{"name":%q,"file_path":%q,"layer":%q}`, docName, filePath, *layer)
	body, err := kbPost(dashboardURL()+"/api/knowledge/documents", payload)
	if err != nil {
		fmt.Fprintf(os.Stderr, "bd kb import-file: %v\n", err)
		os.Exit(1)
	}

	var meta map[string]interface{}
	if err := json.Unmarshal(body, &meta); err != nil {
		fmt.Fprintf(os.Stderr, "bd kb import-file: invalid response: %v\n", err)
		os.Exit(1)
	}

	factCount, _ := meta["fact_count"].(float64)
	slug, _ := meta["slug"].(string)
	fmt.Printf("Imported %q → %d facts (slug: %s)\n", docName, int(factCount), slug)
}

func cmdKBListDocs() {
	body, err := kbGet(dashboardURL() + "/api/knowledge/documents")
	if err != nil {
		fmt.Fprintf(os.Stderr, "bd kb list-docs: %v\n", err)
		os.Exit(1)
	}

	var docs []map[string]interface{}
	if err := json.Unmarshal(body, &docs); err != nil {
		fmt.Fprintf(os.Stderr, "bd kb list-docs: invalid response: %v\n", err)
		os.Exit(1)
	}

	if len(docs) == 0 {
		fmt.Println("No documents imported.")
		return
	}

	for _, d := range docs {
		slug, _ := d["slug"].(string)
		ct, _ := d["content_type"].(string)
		factCount, _ := d["fact_count"].(float64)
		srcURL, _ := d["source_url"].(string)
		srcFile, _ := d["source_file"].(string)

		source := srcURL
		if source == "" {
			source = srcFile
		}
		fmt.Printf("  %-30s  %s  (%d facts)  %s\n", slug, ct, int(factCount), source)
	}
}

func cmdKBCtx7Search(args []string) {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "bd kb ctx7-search: library name required")
		os.Exit(1)
	}
	query := strings.Join(args, " ")

	base := dashboardURL()
	u := fmt.Sprintf("%s/api/knowledge/context7/search?q=%s", base, url.QueryEscape(query))

	body, err := kbGet(u)
	if err != nil {
		fmt.Fprintf(os.Stderr, "bd kb ctx7-search: %v\n", err)
		os.Exit(1)
	}

	var results []map[string]interface{}
	if err := json.Unmarshal(body, &results); err != nil {
		fmt.Fprintf(os.Stderr, "bd kb ctx7-search: invalid response: %v\n", err)
		os.Exit(1)
	}

	if len(results) == 0 {
		fmt.Println("No libraries found.")
		return
	}

	for _, r := range results {
		id, _ := r["id"].(string)
		name, _ := r["title"].(string)
		desc, _ := r["description"].(string)
		trust, _ := r["trustScore"].(float64)
		snippets, _ := r["totalSnippets"].(float64)
		stars, _ := r["stars"].(float64)

		fmt.Printf("  %-35s  %s\n", id, name)
		if trust > 0 || snippets > 0 {
			fmt.Printf("    trust: %.1f  snippets: %d  stars: %d\n", trust, int(snippets), int(stars))
		}
		if desc != "" {
			const maxDescChars = 120
			if len(desc) > maxDescChars {
				desc = desc[:maxDescChars] + "..."
			}
			fmt.Printf("    %s\n", desc)
		}
		fmt.Println()
	}
	fmt.Println("Import with: bd kb import-ctx7 <library-id> [--name <name>] [--query <topic>]")
}

func cmdKBImportCtx7(args []string) {
	fs := flag.NewFlagSet("import-ctx7", flag.ExitOnError)
	name := fs.String("name", "", "document name (defaults to library ID)")
	query := fs.String("query", "", "topic to focus documentation on")
	layer := fs.String("layer", "community", "knowledge layer")
	_ = fs.Parse(args) // ExitOnError: Parse never returns on failure, it os.Exits internally

	if fs.NArg() < 1 {
		fmt.Fprintln(os.Stderr, "bd kb import-ctx7: library ID required (e.g. /vllm-project/vllm)")
		fmt.Fprintln(os.Stderr, "  Use 'bd kb ctx7-search <name>' to find library IDs")
		os.Exit(1)
	}
	libraryID := fs.Arg(0)

	docName := *name
	if docName == "" {
		docName = strings.TrimLeft(libraryID, "/")
		docName = strings.ReplaceAll(docName, "/", "-")
	}

	payload := map[string]string{
		"name":       docName,
		"context7_id": libraryID,
		"layer":      *layer,
	}
	if *query != "" {
		payload["name"] = docName + " (" + *query + ")"
	}

	payloadJSON, _ := json.Marshal(payload)
	body, err := kbPost(dashboardURL()+"/api/knowledge/documents", string(payloadJSON))
	if err != nil {
		fmt.Fprintf(os.Stderr, "bd kb import-ctx7: %v\n", err)
		os.Exit(1)
	}

	var meta map[string]interface{}
	if err := json.Unmarshal(body, &meta); err != nil {
		fmt.Fprintf(os.Stderr, "bd kb import-ctx7: invalid response: %v\n", err)
		os.Exit(1)
	}

	factCount, _ := meta["fact_count"].(float64)
	slug, _ := meta["slug"].(string)
	fmt.Printf("Imported Context7 docs for %q → %d facts (slug: %s)\n", libraryID, int(factCount), slug)
}

func kbGet(url string) ([]byte, error) {
	resp, err := http.Get(url)
	if err != nil {
		return nil, fmt.Errorf("HTTP GET: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, kbMaxResponseBytes))
	if err != nil {
		return nil, fmt.Errorf("reading response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(body))
	}
	return body, nil
}

func kbPost(url, jsonBody string) ([]byte, error) {
	resp, err := http.Post(url, "application/json", strings.NewReader(jsonBody))
	if err != nil {
		return nil, fmt.Errorf("HTTP POST: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, kbMaxResponseBytes))
	if err != nil {
		return nil, fmt.Errorf("reading response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(body))
	}
	return body, nil
}

func urlToName(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "unnamed-document"
	}
	path := strings.TrimRight(u.Path, "/")
	if path == "" {
		return u.Host
	}
	base := filepath.Base(path)
	return strings.TrimSuffix(base, filepath.Ext(base))
}

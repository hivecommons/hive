package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/hivecommons/hive/internal/settingsparity"
)

type row struct {
	Key     string `json:"key"`
	Type    string `json:"type"`
	Default string `json:"default"`
	UI      string `json:"ui"`
	Where   string `json:"where"`
	Notes   string `json:"notes"`
}

func main() {
	format := flag.String("format", "markdown", "markdown or json")
	flag.Parse()
	repoRoot, err := filepath.Abs("..")
	if err != nil {
		fatal(err)
	}
	if _, err := os.Stat(filepath.Join(repoRoot, "src", "pkg", "config", "config.go")); err != nil {
		cwd, _ := os.Getwd()
		repoRoot = filepath.Dir(cwd)
	}
	leaves, err := settingsparity.ConfigLeaves(repoRoot)
	if err != nil {
		fatal(err)
	}
	defaults := exampleDefaults(filepath.Join(repoRoot, "src", "hive.yaml.example"))
	refs := referenceCorpus(repoRoot)
	var rows []row
	for _, leaf := range leaves {
		ui := "none"
		where := ""
		if referenced(refs, leaf.Path) {
			ui = "read/write"
			where = "Settings dialog reference"
		}
		rows = append(rows, row{Key: leaf.Path, Type: leaf.Type, Default: defaults[leaf.Path], UI: ui, Where: where, Notes: notesFor(leaf.Path)})
	}
	if *format == "json" {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(rows); err != nil {
			fatal(err)
		}
		return
	}
	fmt.Println("| key | type | default | exposed-in-UI | where | notes |")
	fmt.Println("| --- | --- | --- | --- | --- | --- |")
	for _, r := range rows {
		fmt.Printf("| `%s` | `%s` | %s | %s | %s | %s |\n", esc(r.Key), esc(r.Type), cell(r.Default), cell(r.UI), cell(r.Where), cell(r.Notes))
	}
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}

func referenceCorpus(repoRoot string) string {
	var b strings.Builder
	paths := []string{filepath.Join(repoRoot, "src", "pkg", "dashboard", "static", "index.html")}
	_ = filepath.WalkDir(filepath.Join(repoRoot, "src", "pkg", "dashboard"), func(path string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() && strings.HasSuffix(path, ".go") {
			paths = append(paths, path)
		}
		return nil
	})
	sort.Strings(paths)
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err == nil {
			b.Write(data)
			b.WriteByte('\n')
		}
	}
	return b.String()
}

func referenced(corpus, path string) bool {
	if strings.Contains(corpus, path) {
		return true
	}
	if strings.Contains(path, "<key>") || strings.Contains(path, "[]") {
		return false
	}
	return false
}

func exampleDefaults(path string) map[string]string {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	defaults := map[string]string{}
	stack := []string{}
	for _, raw := range strings.Split(string(data), "\n") {
		line := strings.TrimRight(raw, " \t")
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") || !strings.Contains(trimmed, ":") {
			continue
		}
		indent := (len(line) - len(strings.TrimLeft(line, " "))) / 2
		for len(stack) > indent {
			stack = stack[:len(stack)-1]
		}
		parts := strings.SplitN(trimmed, ":", 2)
		key := strings.Trim(parts[0], "'\"")
		if strings.HasPrefix(key, "-") || key == "" {
			continue
		}
		if len(stack) == indent {
			stack = append(stack, key)
		} else if len(stack) > indent {
			stack[indent] = key
		} else {
			stack = append(stack, key)
		}
		value := strings.TrimSpace(strings.SplitN(parts[1], "#", 2)[0])
		if value != "" {
			defaults[strings.Join(stack[:indent+1], ".")] = value
		}
	}
	return defaults
}

func notesFor(path string) string {
	lower := strings.ToLower(path)
	switch {
	case strings.Contains(lower, "token"), strings.Contains(lower, "secret"), strings.Contains(lower, "key"), strings.Contains(lower, "pem"), strings.Contains(lower, "password"):
		return "secret/credential — expose as masked or write-only"
	case strings.Contains(lower, "launch_cmd"), strings.Contains(lower, "backend"), strings.Contains(lower, "url"):
		return "may require restart or reconnect"
	case strings.HasPrefix(path, "hub."):
		return "hub-managed hives may render read-only"
	default:
		return ""
	}
}

func cell(s string) string {
	if s == "" {
		return ""
	}
	return esc(s)
}

func esc(s string) string {
	s = strings.ReplaceAll(s, "|", "\\|")
	s = strings.ReplaceAll(s, "\n", " ")
	return s
}

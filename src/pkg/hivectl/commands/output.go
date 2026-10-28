package commands

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"text/tabwriter"

	"gopkg.in/yaml.v3"
)

type printer struct {
	format string
	out    io.Writer
}

func (p printer) print(value any) error {
	switch p.format {
	case "json":
		encoder := json.NewEncoder(p.out)
		encoder.SetIndent("", "  ")
		return encoder.Encode(value)
	case "yaml":
		data, err := yaml.Marshal(value)
		if err != nil {
			return err
		}
		_, err = p.out.Write(data)
		return err
	case "jsonl":
		return p.printJSONL(value)
	case "table":
		return p.printTable(value)
	default:
		return fmt.Errorf("unsupported output format %q; expected table, json, yaml, or jsonl", p.format)
	}
}

func (p printer) printJSONL(value any) error {
	encoder := json.NewEncoder(p.out)
	if values, ok := value.([]any); ok {
		for _, item := range values {
			if err := encoder.Encode(item); err != nil {
				return err
			}
		}
		return nil
	}
	return encoder.Encode(value)
}

func (p printer) printTable(value any) error {
	if text, ok := value.(string); ok {
		_, err := fmt.Fprintln(p.out, compactString(text))
		return err
	}
	writer := tabwriter.NewWriter(p.out, 0, 4, 2, ' ', 0)
	// tabwriter buffers every Write below in memory; the buffer only reaches
	// p.out on Flush, so Flush's error is the one that can genuinely surface a
	// broken pipe or full disk. Capture and return it instead of a bare defer.
	var flushErr error
	defer func() {
		if err := writer.Flush(); err != nil && flushErr == nil {
			flushErr = err
		}
	}()

	switch typed := value.(type) {
	case map[string]any:
		keys := sortedKeys(typed)
		labels := disambiguatedLabels(keys)
		for i, key := range keys {
			_, _ = fmt.Fprintf(writer, "%s\t%s\n", labels[i], compactValue(typed[key])) // buffered by tabwriter; Flush error is captured above
		}
	case []any:
		rows := make([]map[string]any, 0, len(typed))
		columns := map[string]struct{}{}
		for _, item := range typed {
			row, ok := item.(map[string]any)
			if !ok {
				return p.printJSONL(value)
			}
			rows = append(rows, row)
			for key := range row {
				columns[key] = struct{}{}
			}
		}
		headers := make([]string, 0, len(columns))
		for key := range columns {
			headers = append(headers, key)
		}
		sort.Strings(headers)
		_, _ = fmt.Fprintln(writer, strings.Join(disambiguatedLabels(headers), "\t")) // buffered by tabwriter; Flush error is captured above
		for _, row := range rows {
			values := make([]string, len(headers))
			for index, header := range headers {
				values[index] = compactValue(row[header])
			}
			_, _ = fmt.Fprintln(writer, strings.Join(values, "\t")) // buffered by tabwriter; Flush error is captured above
		}
	default:
		data, err := json.MarshalIndent(value, "", "  ")
		if err != nil {
			return err
		}
		_, _ = fmt.Fprintln(writer, string(data)) // buffered by tabwriter; Flush error is captured above
	}
	return flushErr
}

func compactValue(value any) string {
	if value == nil {
		return ""
	}
	switch typed := value.(type) {
	case string:
		return compactString(typed)
	case float64, bool:
		return fmt.Sprint(typed)
	default:
		data, err := json.Marshal(typed)
		if err != nil {
			return fmt.Sprint(typed)
		}
		return string(data)
	}
}

// disambiguatedLabels compacts each key for a table cell and suffixes " (n)"
// onto any label that a previous key already compacted to, so distinct keys
// that differ only by control characters stay distinguishable in output.
func disambiguatedLabels(keys []string) []string {
	labels := make([]string, len(keys))
	seen := map[string]int{}
	for i, key := range keys {
		label := compactString(key)
		seen[label]++
		if n := seen[label]; n > 1 {
			label = fmt.Sprintf("%s (%d)", label, n)
		}
		labels[i] = label
	}
	return labels
}

// compactString makes a server- or index-derived string safe for a single
// terminal table cell: newlines become a literal \n, tabs become spaces so
// they cannot shift tabwriter columns, and every other C0/C1 control
// character (ESC, BEL, CR, DEL, ...) is dropped so third-party text indexed
// into search cannot carry ANSI/OSC sequences into an operator terminal.
func compactString(s string) string {
	s = strings.ReplaceAll(s, "\n", "\\n")
	return strings.Map(func(r rune) rune {
		switch {
		case r == '\t':
			return ' '
		case r < 0x20, r == 0x7f, r >= 0x80 && r <= 0x9f:
			return -1
		}
		return r
	}, s)
}

// decodeResult re-encodes a decoded `any` API result into the typed struct v
// so table printers can work with fields instead of map lookups.
func decodeResult(result any, v any, what string) error {
	data, err := json.Marshal(result)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, v); err != nil {
		return fmt.Errorf("decoding %s response: %w", what, err)
	}
	return nil
}

func sortedKeys(values map[string]any) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

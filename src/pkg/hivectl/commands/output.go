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
		_, err := fmt.Fprintln(p.out, text)
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
		for _, key := range keys {
			_, _ = fmt.Fprintf(writer, "%s\t%s\n", key, compactValue(typed[key])) // buffered by tabwriter; Flush error is captured above
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
		_, _ = fmt.Fprintln(writer, strings.Join(headers, "\t")) // buffered by tabwriter; Flush error is captured above
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
		return strings.ReplaceAll(typed, "\n", "\\n")
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

func sortedKeys(values map[string]any) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

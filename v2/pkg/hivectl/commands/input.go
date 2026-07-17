package commands

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

const maxAPIInputBytes = 1 << 20

func readInput(reader io.Reader, file string, stdin bool) ([]byte, error) {
	if file != "" && stdin {
		return nil, &usageError{message: "use only one of --file or --stdin"}
	}
	if file == "" && !stdin {
		return nil, &usageError{message: "one of --file or --stdin is required"}
	}
	if stdin {
		data, err := io.ReadAll(io.LimitReader(reader, maxAPIInputBytes+1))
		if err != nil {
			return nil, err
		}
		if len(data) > maxAPIInputBytes {
			return nil, &usageError{message: "input exceeds the Hive API 1 MiB request limit"}
		}
		if len(strings.TrimSpace(string(data))) == 0 {
			return nil, &usageError{message: "stdin was empty"}
		}
		return data, nil
	}
	info, err := os.Stat(file)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", file, err)
	}
	if info.Size() > maxAPIInputBytes {
		return nil, &usageError{message: "input exceeds the Hive API 1 MiB request limit"}
	}
	data, err := os.ReadFile(file)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", file, err)
	}
	return data, nil
}

func decodeStructured(data []byte) (any, error) {
	var value any
	if json.Unmarshal(data, &value) == nil {
		return normalizeYAMLValue(value), nil
	}
	if err := yaml.Unmarshal(data, &value); err != nil {
		return nil, fmt.Errorf("input is neither valid JSON nor YAML: %w", err)
	}
	return normalizeYAMLValue(value), nil
}

func normalizeYAMLValue(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		result := make(map[string]any, len(typed))
		for key, child := range typed {
			result[key] = normalizeYAMLValue(child)
		}
		return result
	case map[any]any:
		result := make(map[string]any, len(typed))
		for key, child := range typed {
			result[fmt.Sprint(key)] = normalizeYAMLValue(child)
		}
		return result
	case []any:
		for index := range typed {
			typed[index] = normalizeYAMLValue(typed[index])
		}
	}
	return value
}

func readStructuredInput(reader io.Reader, file string, stdin bool) (any, error) {
	data, err := readInput(reader, file, stdin)
	if err != nil {
		return nil, err
	}
	return decodeStructured(data)
}

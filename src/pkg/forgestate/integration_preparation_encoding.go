package forgestate

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
)

// CanonicalIntegrationPreparation emits the same bounded canonical JSON/digest as
// the trusted Node materializer. In particular it does not HTML-escape strings
// or confuse a fresh observer generation with an immutable candidate identity.
func CanonicalIntegrationPreparation(request any) (string, string, error) {
	raw, err := json.Marshal(request)
	if err != nil {
		return "", "", err
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return "", "", err
	}
	var output bytes.Buffer
	if err := writeIntegrationCanonical(&output, value); err != nil {
		return "", "", err
	}
	if output.Len() > 32<<10 {
		return "", "", fmt.Errorf("bounded preparation request exceeded")
	}
	sum := sha256.Sum256(output.Bytes())
	return output.String(), hex.EncodeToString(sum[:]), nil
}

func writeIntegrationString(output *bytes.Buffer, value string) {
	output.WriteByte('"')
	for _, character := range value {
		switch character {
		case '"', '\\':
			output.WriteByte('\\')
			output.WriteRune(character)
		case '\b':
			output.WriteString(`\b`)
		case '\f':
			output.WriteString(`\f`)
		case '\n':
			output.WriteString(`\n`)
		case '\r':
			output.WriteString(`\r`)
		case '\t':
			output.WriteString(`\t`)
		default:
			if character < ' ' {
				fmt.Fprintf(output, `\u%04x`, character)
			} else {
				output.WriteRune(character)
			}
		}
	}
	output.WriteByte('"')
}

func writeIntegrationCanonical(output *bytes.Buffer, value any) error {
	switch typed := value.(type) {
	case nil:
		output.WriteString("null")
	case bool:
		output.WriteString(strconv.FormatBool(typed))
	case string:
		writeIntegrationString(output, typed)
	case json.Number:
		number, err := typed.Int64()
		if err != nil || number < 0 || number > 1<<53-1 {
			return fmt.Errorf("preparation number is not a safe protocol integer")
		}
		output.WriteString(strconv.FormatInt(number, 10))
	case []any:
		output.WriteByte('[')
		for i, item := range typed {
			if i > 0 {
				output.WriteByte(',')
			}
			if err := writeIntegrationCanonical(output, item); err != nil {
				return err
			}
		}
		output.WriteByte(']')
	case map[string]any:
		keys := make([]string, 0, len(typed))
		for key := range typed {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		output.WriteByte('{')
		for i, key := range keys {
			if i > 0 {
				output.WriteByte(',')
			}
			writeIntegrationString(output, key)
			output.WriteByte(':')
			if err := writeIntegrationCanonical(output, typed[key]); err != nil {
				return err
			}
		}
		output.WriteByte('}')
	default:
		return fmt.Errorf("unsupported canonical preparation value")
	}
	return nil
}

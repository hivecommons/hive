package dashboard

import (
	"encoding/json"
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/kubestellar/hive/pkg/tokens"
)

// TestOpenAPISchemaFieldsExistOnGoTypes is the #5077 guard.
//
// TestOpenAPISpecCoversEveryRegisteredRoute (openapi_route_parity_test.go)
// proves every registered route is DOCUMENTED. It says nothing about whether
// what the documentation claims is TRUE. That blind spot is exactly how #5077
// happened: /api/status's governor object was published as
// {mode, queue, budgetPct} while the server has always sent
// dashboard.FrontendGovernor {active, mode, issues, prs, thresholds, nextKick}.
// Both `queue` and `budgetPct` were pure invention, the route guard was green
// throughout, and three TUI client tasks carried an acceptance criterion
// ("mirror the spec, do not invent fields") that mirroring the spec could not
// satisfy.
//
// This guard closes the direction that actually bit: a spec property that
// corresponds to NO field on the Go type the handler marshals. It is
// deliberately one-directional. Requiring the converse — every Go field
// documented — would fail immediately and permanently on StatusPayload, which
// carries far more fields than any client consumes and which this spec has
// never attempted to enumerate exhaustively. A guard that cannot be made green
// gets deleted or blanket-skipped, and then it protects nothing; a guard that
// only forbids INVENTION is one that can hold.
//
// Nested objects are checked recursively wherever the spec descends into
// `properties`, so FrontendThresholds and tokens.SessionSummary are covered
// through their parents. A spec object that stops at `type: object` with no
// properties (the honest way to say "untyped map[string]any") is skipped, as
// there is nothing to verify.
func TestOpenAPISchemaFieldsExistOnGoTypes(t *testing.T) {
	// Each case pins one documented response schema to the Go value the
	// handler actually passes to jsonResponse. Adding an endpoint here is the
	// cheapest possible way to extend this guard's reach.
	cases := []struct {
		name string
		path string
		// goType is the type the handler marshals for the 200 response.
		goType reflect.Type
		// degenerate lists spec properties that belong to an alternative
		// response shape the handler emits instead of goType (never alongside
		// it), so they legitimately have no field on goType. Each entry must
		// name the handler branch that produces it.
		degenerate map[string]string
	}{
		{
			name:   "GET /api/status",
			path:   "/api/status",
			goType: reflect.TypeOf(StatusPayload{}),
			degenerate: map[string]string{
				"status": "handleStatus returns the bare object {\"status\":\"initializing\"} " +
					"before the first status build completes, instead of a StatusPayload.",
			},
		},
		{
			name:   "GET /api/tokens",
			path:   "/api/tokens",
			goType: reflect.TypeOf(tokens.AggregateSummary{}),
			degenerate: map[string]string{
				"status": "handleTokens returns the bare object {\"status\":\"no_collector\"} " +
					"when no token collector is wired, instead of an AggregateSummary.",
			},
		},
		{
			name:   "GET /api/audit entries[]",
			path:   "/api/audit",
			goType: reflect.TypeOf(AuditEntry{}),
		},
	}

	raw, err := os.ReadFile(openAPISpecPath)
	if err != nil {
		t.Fatalf("reading %s: %v", openAPISpecPath, err)
	}
	var doc struct {
		Paths map[string]map[string]struct {
			Responses map[string]struct {
				Content map[string]struct {
					Schema map[string]any `json:"schema"`
				} `json:"content"`
			} `json:"responses"`
		} `json:"paths"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parsing %s: %v", openAPISpecPath, err)
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			op, ok := doc.Paths[tc.path]["get"]
			if !ok {
				t.Fatalf("%s documents no GET %s; this guard's case list has gone stale",
					openAPISpecPath, tc.path)
			}
			schema := op.Responses["200"].Content["application/json"].Schema
			if len(schema) == 0 {
				t.Fatalf("GET %s has no application/json 200 schema to check", tc.path)
			}
			// /api/audit's rows are the thing typed by AuditEntry; descend to
			// them rather than checking the {entries: [...]} envelope, which
			// has no Go struct of its own (handleAuditLog builds it inline).
			if tc.path == "/api/audit" {
				entries, ok := schema["properties"].(map[string]any)["entries"].(map[string]any)
				if !ok {
					t.Fatalf("GET /api/audit 200 schema has no `entries` property")
				}
				schema, ok = entries["items"].(map[string]any)
				if !ok {
					t.Fatalf("GET /api/audit `entries` has no `items` schema")
				}
			}

			var problems []string
			checkSchemaAgainstType(t, schema, tc.goType, tc.path, tc.degenerate, &problems)
			if len(problems) > 0 {
				sort.Strings(problems)
				t.Fatalf("%s documents field(s) that do not exist on the Go type the handler "+
					"marshals — the #5077 defect class (%d problem(s)):\n  %s",
					openAPISpecPath, len(problems), strings.Join(problems, "\n  "))
			}
		})
	}
}

// checkSchemaAgainstType walks a spec object schema alongside the Go type it
// claims to describe, recording every documented property with no
// corresponding JSON-marshalled field. where is a human-readable breadcrumb
// used only in failure messages.
func checkSchemaAgainstType(t *testing.T, schema map[string]any, goType reflect.Type,
	where string, degenerate map[string]string, problems *[]string) {
	t.Helper()

	for goType.Kind() == reflect.Pointer {
		goType = goType.Elem()
	}
	props, ok := schema["properties"].(map[string]any)
	if !ok || len(props) == 0 {
		// An object documented without properties is the honest way to
		// describe a map[string]any / genuinely dynamic payload. Nothing to
		// verify, and demanding properties here would push authors toward
		// inventing a shape — the very thing this guard exists to prevent.
		return
	}
	if goType.Kind() != reflect.Struct {
		// e.g. the spec descends into an object where Go has map[string]any.
		// The keys are data, not fields; not checkable.
		return
	}

	fields := jsonFieldsOfStruct(goType)
	for name, sub := range props {
		field, known := fields[name]
		if !known {
			if _, excused := degenerate[name]; excused {
				continue
			}
			*problems = append(*problems, where+"."+name+" is documented but "+
				goType.String()+" marshals no such key — remove it, correct the name, or "+
				"(if it belongs to an alternative response shape the handler emits) declare "+
				"it in the case's degenerate map with the handler branch that produces it")
			continue
		}
		subSchema, ok := sub.(map[string]any)
		if !ok {
			continue
		}
		// Descend through arrays to the item schema so []SessionSummary and
		// friends are checked against their element type.
		fieldType := field
		itemSchema := subSchema
		for itemSchema["type"] == "array" {
			next, ok := itemSchema["items"].(map[string]any)
			if !ok {
				break
			}
			itemSchema = next
			for fieldType.Kind() == reflect.Pointer {
				fieldType = fieldType.Elem()
			}
			if fieldType.Kind() != reflect.Slice && fieldType.Kind() != reflect.Array {
				break
			}
			fieldType = fieldType.Elem()
		}
		// additionalProperties describes a map's VALUE type; descend into it
		// so by_agent_detail's bucket schema is checked against
		// tokens.AgentModelBucket.
		if ap, ok := itemSchema["additionalProperties"].(map[string]any); ok {
			ft := fieldType
			for ft.Kind() == reflect.Pointer {
				ft = ft.Elem()
			}
			if ft.Kind() == reflect.Map {
				checkSchemaAgainstType(t, ap, ft.Elem(), where+"."+name+"[*]", nil, problems)
			}
			continue
		}
		checkSchemaAgainstType(t, itemSchema, fieldType, where+"."+name, nil, problems)
	}
}

// jsonFieldsOfStruct maps the JSON key each exported field of goType marshals
// to, to that field's type — resolving embedded structs the same way
// encoding/json promotes them.
func jsonFieldsOfStruct(goType reflect.Type) map[string]reflect.Type {
	out := make(map[string]reflect.Type)
	for i := 0; i < goType.NumField(); i++ {
		f := goType.Field(i)
		tag := f.Tag.Get("json")
		if tag == "-" {
			continue
		}
		name, _, _ := strings.Cut(tag, ",")
		if f.Anonymous && name == "" {
			// Embedded without a tag: encoding/json promotes its fields.
			et := f.Type
			for et.Kind() == reflect.Pointer {
				et = et.Elem()
			}
			if et.Kind() == reflect.Struct {
				for k, v := range jsonFieldsOfStruct(et) {
					out[k] = v
				}
			}
			continue
		}
		if !f.IsExported() {
			continue
		}
		if name == "" {
			name = f.Name
		}
		out[name] = f.Type
	}
	return out
}

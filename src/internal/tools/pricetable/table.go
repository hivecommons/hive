package main

import (
	"errors"
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"math/big"
	"sort"
	"strconv"
	"strings"
)

// Field names of tokens.ModelPrice, in the order the table writes them.
const (
	fieldInput      = "InputPerMTok"
	fieldOutput     = "OutputPerMTok"
	fieldCacheRead  = "CacheReadPerMTok"
	fieldCacheWrite = "CacheWritePerMTok"
)

var priceFields = []string{fieldInput, fieldOutput, fieldCacheRead, fieldCacheWrite}

// literal is one numeric literal in the source: its exact value and byte span.
type literal struct {
	Value *big.Rat
	Text  string
	Start int
	End   int
}

// entry is one modelPrices row keyed by its normalised model id.
type entry struct {
	Key    string
	Fields map[string]literal
}

// table is the parsed price table: the date constant and every row.
type table struct {
	Date        string
	DateLiteral literal
	Entries     []entry
}

// entry returns the row for key, if present.
func (t *table) entry(key string) (entry, bool) {
	for _, e := range t.Entries {
		if e.Key == key {
			return e, true
		}
	}
	return entry{}, false
}

// parseTable extracts priceTableDate and modelPrices from pricing.go source.
func parseTable(src []byte) (*table, error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "pricing.go", src, parser.ParseComments)
	if err != nil {
		return nil, err
	}
	offset := func(p token.Pos) int { return fset.Position(p).Offset }

	t := &table{}
	for _, decl := range file.Decls {
		gd, ok := decl.(*ast.GenDecl)
		if !ok {
			continue
		}
		for _, spec := range gd.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok || len(vs.Names) != 1 || len(vs.Values) != 1 {
				continue
			}
			switch vs.Names[0].Name {
			case "priceTableDate":
				lit, ok := vs.Values[0].(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					return nil, errors.New("priceTableDate is not a string literal")
				}
				date, err := strconv.Unquote(lit.Value)
				if err != nil {
					return nil, fmt.Errorf("priceTableDate: %w", err)
				}
				t.Date = date
				t.DateLiteral = literal{Text: lit.Value, Start: offset(lit.Pos()), End: offset(lit.End())}
			case "modelPrices":
				cl, ok := vs.Values[0].(*ast.CompositeLit)
				if !ok {
					return nil, errors.New("modelPrices is not a composite literal")
				}
				entries, err := parseEntries(cl, offset)
				if err != nil {
					return nil, err
				}
				t.Entries = entries
			}
		}
	}
	if t.Date == "" {
		return nil, errors.New("priceTableDate not found")
	}
	if len(t.Entries) == 0 {
		return nil, errors.New("modelPrices not found or empty")
	}
	return t, nil
}

// parseEntries walks the map literal's key/value pairs.
func parseEntries(cl *ast.CompositeLit, offset func(token.Pos) int) ([]entry, error) {
	var entries []entry
	for _, elt := range cl.Elts {
		kv, ok := elt.(*ast.KeyValueExpr)
		if !ok {
			return nil, errors.New("modelPrices element is not key: value")
		}
		keyLit, ok := kv.Key.(*ast.BasicLit)
		if !ok || keyLit.Kind != token.STRING {
			return nil, errors.New("modelPrices key is not a string literal")
		}
		key, err := strconv.Unquote(keyLit.Value)
		if err != nil {
			return nil, err
		}
		val, ok := kv.Value.(*ast.CompositeLit)
		if !ok {
			return nil, fmt.Errorf("%s: value is not a ModelPrice literal", key)
		}
		e := entry{Key: key, Fields: make(map[string]literal, len(priceFields))}
		for _, f := range val.Elts {
			fkv, ok := f.(*ast.KeyValueExpr)
			if !ok {
				return nil, fmt.Errorf("%s: ModelPrice field is not named", key)
			}
			name, ok := fkv.Key.(*ast.Ident)
			if !ok {
				return nil, fmt.Errorf("%s: ModelPrice field key is not an identifier", key)
			}
			lit, ok := fkv.Value.(*ast.BasicLit)
			if !ok || (lit.Kind != token.FLOAT && lit.Kind != token.INT) {
				return nil, fmt.Errorf("%s.%s: value is not a numeric literal", key, name.Name)
			}
			r, ok := new(big.Rat).SetString(lit.Value)
			if !ok {
				return nil, fmt.Errorf("%s.%s: cannot parse %q", key, name.Name, lit.Value)
			}
			e.Fields[name.Name] = literal{Value: r, Text: lit.Value, Start: offset(lit.Pos()), End: offset(lit.End())}
		}
		for _, name := range priceFields {
			if _, ok := e.Fields[name]; !ok {
				return nil, fmt.Errorf("%s: missing %s", key, name)
			}
		}
		entries = append(entries, e)
	}
	return entries, nil
}

// edit is one byte-range replacement in the source.
type edit struct {
	Start, End int
	Text       string
}

// applyChanges rewrites the changed literals and the date in place, touching
// nothing else, then gofmt's the result so column alignment stays canonical.
func applyChanges(src []byte, t *table, r *result) ([]byte, error) {
	var edits []edit
	if r.NewDate != "" && r.NewDate != t.Date {
		edits = append(edits, edit{t.DateLiteral.Start, t.DateLiteral.End, strconv.Quote(r.NewDate)})
	}
	for _, c := range r.Changes {
		e, ok := t.entry(c.Key)
		if !ok {
			return nil, fmt.Errorf("change for unknown key %q", c.Key)
		}
		for _, name := range priceFields {
			fc, ok := c.Fields[name]
			if !ok {
				continue
			}
			lit := e.Fields[name]
			edits = append(edits, edit{lit.Start, lit.End, formatPrice(fc.New)})
		}
	}
	// Apply from the end so earlier offsets stay valid.
	sort.Slice(edits, func(i, j int) bool { return edits[i].Start > edits[j].Start })
	out := make([]byte, len(src))
	copy(out, src)
	for _, ed := range edits {
		out = append(out[:ed.Start], append([]byte(ed.Text), out[ed.End:]...)...)
	}
	formatted, err := format.Source(out)
	if err != nil {
		return nil, fmt.Errorf("gofmt: %w", err)
	}
	return formatted, nil
}

// maxPriceDecimals bounds the written precision: OpenRouter publishes some
// per-token rates as repeating decimals, which would otherwise never settle.
const maxPriceDecimals = 6

// minPriceDecimals keeps the table's "5.00" style for whole-dollar rates.
const minPriceDecimals = 2

// formatPrice renders a per-MTok rate as the table's Go float literal: at most
// maxPriceDecimals, trailing zeros trimmed, never fewer than minPriceDecimals.
func formatPrice(v *big.Rat) string {
	s := v.FloatString(maxPriceDecimals)
	whole, frac, _ := strings.Cut(s, ".")
	frac = strings.TrimRight(frac, "0")
	for len(frac) < minPriceDecimals {
		frac += "0"
	}
	return whole + "." + frac
}

// roundPrice snaps a rate to the precision formatPrice writes, so comparing
// a stored literal with a freshly fetched value is stable run to run.
func roundPrice(v *big.Rat) *big.Rat {
	r, _ := new(big.Rat).SetString(v.FloatString(maxPriceDecimals))
	return r
}

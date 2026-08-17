package hooks

import (
	"bytes"
	"strings"
	"text/template"
)

// RenderTemplate evaluates a template string against the event data.
func RenderTemplate(tmplStr string, event Event) (string, error) {
	if !strings.Contains(tmplStr, "{{") {
		return tmplStr, nil
	}

	tmpl, err := template.New("hook").Option("missingkey=zero").Parse(tmplStr)
	if err != nil {
		return tmplStr, err
	}

	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, event); err != nil {
		return tmplStr, err
	}

	return buf.String(), nil
}

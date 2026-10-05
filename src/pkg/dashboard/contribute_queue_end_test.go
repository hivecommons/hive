package dashboard

import (
	"strings"
	"testing"

	"golang.org/x/net/html"
)

// The end marker must scroll with the rows, but remain outside the row mount
// whose innerHTML is replaced on polls and filter changes.
func TestQueueEndScrollsAfterRows(t *testing.T) {
	body := renderContributePage(t)
	doc, err := html.Parse(strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	var rows, end *html.Node
	var visit func(*html.Node)
	visit = func(n *html.Node) {
		for _, attr := range n.Attr {
			if attr.Key == "id" {
				switch attr.Val {
				case "cc-queue":
					rows = n
				case "cc-q-end":
					end = n
				}
			}
		}
		for child := n.FirstChild; child != nil; child = child.NextSibling {
			visit(child)
		}
	}
	visit(doc)
	if rows == nil || end == nil {
		t.Fatal("queue rows or end marker missing")
	}
	if rows.Parent != end.Parent {
		t.Fatal("rows and end marker must share a scroll container")
	}
	scrollClass := false
	for _, attr := range rows.Parent.Attr {
		if attr.Key == "class" {
			for _, class := range strings.Fields(attr.Val) {
				if class == "cc-queue-scroll" {
					scrollClass = true
				}
			}
		}
	}
	if !scrollClass || !strings.Contains(body, ".cc-queue-scroll{max-height:560px;overflow-y:auto}") {
		t.Fatal("shared container must own the queue overflow")
	}
	next := rows.NextSibling
	for next != nil && next.Type != html.ElementNode {
		next = next.NextSibling
	}
	if next != end {
		t.Fatal("end marker must follow the rows inside the scroll container")
	}
}

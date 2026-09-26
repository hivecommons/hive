package worksource

import (
	"strconv"
	"strings"

	"github.com/hivecommons/hive/pkg/github"
)

// WorkItemContext is the durable, source-neutral prompt/dashboard description
// of the item that caused a Spektacular run. Repo is the target implementation
// repository; SourceType/ExternalID/URL identify the planning system item.
type WorkItemContext struct {
	SourceType string `json:"source_type,omitempty"`
	Repo       string `json:"repo,omitempty"`
	ExternalID string `json:"external_id,omitempty"`
	Number     int    `json:"number,omitempty"`
	Title      string `json:"title,omitempty"`
	Body       string `json:"body,omitempty"`
	URL        string `json:"url,omitempty"`
}

func (c WorkItemContext) Normalized() WorkItemContext {
	c.SourceType = strings.TrimSpace(c.SourceType)
	c.Repo = strings.TrimSpace(c.Repo)
	c.ExternalID = strings.TrimSpace(c.ExternalID)
	c.Title = strings.TrimSpace(c.Title)
	c.Body = strings.TrimSpace(c.Body)
	c.URL = strings.TrimSpace(c.URL)
	if c.SourceType == "" {
		c.SourceType = "github"
	}
	if c.ExternalID == "" && c.Number > 0 {
		c.ExternalID = strconv.Itoa(c.Number)
	}
	return c
}

func (c WorkItemContext) Ref() Ref {
	c = c.Normalized()
	return Ref{SourceType: c.SourceType, Repo: c.Repo, ExternalID: c.ExternalID, Number: c.Number, URL: c.URL}
}

func WorkItemContextFromRef(ref Ref, title string) WorkItemContext {
	return WorkItemContext{
		SourceType: ref.SourceType,
		Repo:       ref.Repo,
		ExternalID: ref.ExternalID,
		Number:     ref.Number,
		Title:      title,
		URL:        ref.URL,
	}.Normalized()
}

func WorkItemContextFromIssue(issue Issue) WorkItemContext {
	return WorkItemContext{
		SourceType: issue.SourceType,
		Repo:       issue.Repo,
		ExternalID: issue.ExternalID,
		Number:     issue.Number,
		Title:      issue.Title,
		Body:       issue.Body,
		URL:        issue.URL,
	}.Normalized()
}

func WorkItemContextFromGitHubIssue(issue github.Issue) WorkItemContext {
	return WorkItemContext{
		SourceType: issue.SourceType,
		Repo:       issue.Repo,
		ExternalID: issue.ExternalID,
		Number:     issue.Number,
		Title:      issue.Title,
		Body:       issue.Body,
		URL:        issue.URL,
	}.Normalized()
}

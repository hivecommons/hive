package betty

import (
	"context"
	"encoding/json"
	"net/url"
	"regexp"
	"strings"
)

// Definitions and entities from Wikipedia: opensearch finds the page title
// for the topic, then the REST summary gives its lead paragraph.

const (
	// wikiMaxTopicWords is the longest topic looked up; longer questions are
	// not entity lookups and go to the general web search.
	wikiMaxTopicWords = 6
	// wikipediaTitles is how many opensearch titles are asked for; the
	// second is used when the first is a disambiguation page.
	wikipediaTitles = "2"
	// wikipediaDisambiguation is the summary type of a disambiguation page.
	wikipediaDisambiguation = "disambiguation"
	// opensearchTitlesIndex is where the titles sit in an opensearch reply:
	// [query, [titles], [descriptions], [urls]].
	opensearchTitlesIndex = 1
)

// Definition question shapes; the first group is the topic.
var definitionShapes = []*regexp.Regexp{
	regexp.MustCompile(`(?i)^what does (.+?) (?:stand for|mean)$`),
	regexp.MustCompile(`(?i)^(?:what|who)(?:'s|s| is| are| was| were) (?:a |an |the )?(.+)$`),
	// "tell me what the capital of Australia is", after cleaning.
	regexp.MustCompile(`(?i)^(?:what|who) (?:a |an |the )?(.+?) (?:is|are|was|were|means)$`),
	regexp.MustCompile(`(?i)^(?:define|explain|describe) (?:what (?:a |an |the )?)?(.+?)(?: is| are| means)?$`),
	regexp.MustCompile(`(?i)^(?:the )?(?:meaning|definition) of (.+)$`),
}

// notTopicWords mark a "what is ..." question that is not about a named
// thing: it points at the speaker, the moment or a comparison.
var notTopicWords = map[string]bool{
	"you": true, "your": true, "yours": true, "i": true, "my": true, "me": true, "we": true, "our": true, "us": true,
	"it": true, "its": true, "this": true, "that": true, "these": true, "those": true, "he": true, "she": true,
	"they": true, "them": true, "here": true, "there": true, "today": true, "now": true, "tomorrow": true,
	"yesterday": true, "up": true, "new": true, "next": true, "going": true, "happening": true, "time": true,
	"date": true, "latest": true, "newest": true, "best": true, "difference": true, "status": true, "plan": true,
	"news": true, "weather": true,
}

// definitionTopic reports whether a cleaned question asks what or who
// something is, and the topic to look up.
func definitionTopic(c cleanedQuery) (topic string, ok bool) {
	q := strings.TrimSpace(c.Query)
	if c.About {
		topic = q
	} else {
		for _, re := range definitionShapes {
			if m := re.FindStringSubmatch(q); m != nil {
				topic = strings.TrimSpace(m[1])
				break
			}
		}
	}
	w := words(topic)
	if len(w) == 0 || len(w) > wikiMaxTopicWords {
		return "", false
	}
	for _, x := range w {
		if notTopicWords[strings.Trim(x, "'")] {
			return "", false
		}
	}
	return topic, true
}

type wikipediaSummary struct {
	Type        string `json:"type"`
	Title       string `json:"title"`
	Description string `json:"description"`
	Extract     string `json:"extract"`
	ContentURLs struct {
		Desktop struct {
			Page string `json:"page"`
		} `json:"desktop"`
	} `json:"content_urls"`
}

// wikipediaTitlesFor is the page titles opensearch finds for a topic.
func (d DirectRetriever) wikipediaTitlesFor(ctx context.Context, topic string) []string {
	u := base(d.WikipediaURL, defaultWikipediaURL) + "/w/api.php?action=opensearch&format=json&namespace=0&redirects=resolve&limit=" +
		wikipediaTitles + "&search=" + url.QueryEscape(topic)
	var raw []json.RawMessage
	if !d.getJSON(ctx, u, &raw) || len(raw) <= opensearchTitlesIndex {
		return nil
	}
	var titles []string
	if json.Unmarshal(raw[opensearchTitlesIndex], &titles) != nil {
		return nil
	}
	return titles
}

// wikipedia is the lead paragraph of the page that best matches a topic.
func (d DirectRetriever) wikipedia(ctx context.Context, topic string) []Snippet {
	for _, title := range d.wikipediaTitlesFor(ctx, topic) {
		if ctx.Err() != nil {
			return nil
		}
		var s wikipediaSummary
		u := base(d.WikipediaURL, defaultWikipediaURL) + "/api/rest_v1/page/summary/" +
			url.PathEscape(strings.ReplaceAll(strings.TrimSpace(title), " ", "_")) + "?redirect=true"
		if !d.getJSON(ctx, u, &s) || s.Type == wikipediaDisambiguation {
			continue
		}
		extract := stripHTML(s.Extract)
		if extract == "" {
			continue
		}
		name := stripHTML(s.Title)
		if desc := stripHTML(s.Description); desc != "" {
			name += ": " + desc
		}
		text := clip(extract, maxDirectTextRunes) + " According to " + SourceWikipedia
		pageURL := ""
		if link, err := url.Parse(s.ContentURLs.Desktop.Page); err == nil && link.Scheme == "https" && link.Host != "" {
			text += " (" + clip(sanitize(link.String()), maxSnippetRunes) + ")"
			pageURL = link.String()
		}
		return []Snippet{{Source: SourceWikipedia, Title: clip(name, maxSnippetRunes), Text: text + ".", URL: pageURL}}
	}
	return nil
}

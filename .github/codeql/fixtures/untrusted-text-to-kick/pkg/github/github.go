// Package github is a stub of the hive types the query reads: only the
// fields it treats as untrusted, plus one it does not.
package github

type Issue struct {
	Repo   string
	Number int
	Title  string
	Body   string
}

type PullRequest struct {
	Number int
	Title  string
	Body   string
}

type ThreadComment struct {
	Author string
	Body   string
}

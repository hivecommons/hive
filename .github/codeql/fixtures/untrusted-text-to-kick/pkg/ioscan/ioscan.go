// Package ioscan is a stub of the enforcement entry point. It returns the
// text unchanged so that only the query's barrier, not the stub, stops the
// flow.
package ioscan

type Source struct{ Surface string }

type Policy struct{}

type Verdict struct{ Blocked bool }

func EnforceInputFrom(text string, src Source, p Policy) (string, Verdict) {
	return text, Verdict{}
}

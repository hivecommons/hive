package config

import "testing"

// review.all_authors is a deprecated no-op (hivecommons/hive#12974): every
// open PR is reviewed whenever review is on. The knob is still accepted so
// existing hive.yaml files load, and a set value is reported once.

func TestWarnDeprecatedReviewAllAuthors(t *testing.T) {
	var nilCfg *Config
	if nilCfg.WarnDeprecatedReviewAllAuthors() {
		t.Fatal("nil config reported all_authors set")
	}
	if (&Config{}).WarnDeprecatedReviewAllAuthors() {
		t.Fatal("unset all_authors reported as set")
	}
	cfg := &Config{Review: ReviewConfig{AllAuthors: true}}
	if !cfg.WarnDeprecatedReviewAllAuthors() {
		t.Fatal("set all_authors not reported")
	}
	if !cfg.Review.AllAuthors {
		t.Fatal("deprecation warning must leave the stored value alone for audit/UI continuity")
	}
}

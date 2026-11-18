// Package reviewpreflight gathers the facts the hive puts in front of a PR
// reviewer before it reads the diff (hivecommons/hive#19069, #19106): the
// documents the repository itself points reviewers to, operator-defined named
// rules, and, for dependencies the PR adds or bumps, vulnerability advisories
// and publish dates.
//
// Rule documents are not guessed from file names. Discovery starts from the
// standard entry files a repository has (README, CONTRIBUTING, agent
// instruction files, docs index pages, CODEOWNERS for context) and follows the
// repository-relative links and file mentions in them, within a depth and byte
// budget, recording how each document was found.
//
// Everything here is evidence, not verdicts. A publish date carries no verdict
// unless the hive's config declares a release-age policy. The output is a prompt section of
// identifiers, versions, dates and paths; it carries no free text from third
// parties, so a registry cannot inject instructions into a review. A check that
// cannot run says so ("could not check: offline"); it never silently passes.
//
// Adding an ecosystem is one entry in the Ecosystems table (see ecosystems.go)
// plus a test case in ecosystems_test.go; docs/review-swarm.md lists the
// current table.
package reviewpreflight

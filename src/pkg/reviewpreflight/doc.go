// Package reviewpreflight gathers the facts the hive puts in front of a PR
// reviewer before it reads the diff (hivecommons/hive#19069): the repository's
// own rule documents, operator-defined named rules, and, for dependencies the
// PR adds or bumps, vulnerability advisories and release age.
//
// Everything here is evidence, not verdicts. The output is a prompt section of
// identifiers, versions, dates and paths; it carries no free text from third
// parties, so a registry cannot inject instructions into a review. A check that
// cannot run says so ("could not check: offline"); it never silently passes.
//
// Adding an ecosystem is one entry in the Ecosystems table (see ecosystems.go)
// plus a test case in ecosystems_test.go; docs/review-swarm.md lists the
// current table.
package reviewpreflight

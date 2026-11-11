// Package workflowmodel is the declared workflow model behind hivecommons/hive#17620.
//
// A Model is a typed, versioned description of how work moves through a
// repository: states (sequential, parallel regions, final), the transitions
// and guards between them, who owns each state, and the lanes agents may
// work in. Ingesters under this package (githubactions, tekton) read a
// repository's CI definitions deterministically and fill the model's
// execution graph; render derives diagrams and Activity Monitor columns
// from the model.
//
// Nothing here is wired into the stage engine yet: a hive with no bound
// model behaves exactly as before (RFC phase 1 is ingest, store and view).
package workflowmodel

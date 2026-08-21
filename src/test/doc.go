// Package test contains the inception e2e/regression suite.
//
// All tests in this package are real-network integration tests against a
// live hive instance and are guarded by the `integration` build tag, so a
// plain `go test ./...` compiles this package but runs nothing.
//
// To run the suite:
//
//	HIVE_URL=http://<host>:<port> HIVE_TOKEN=<token> go test -tags integration ./test/...
//
// The suite is skipped (exit 0) if HIVE_URL is unset or the endpoint does
// not answer a fast TCP dial.
package test

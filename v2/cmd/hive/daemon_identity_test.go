package main

import "strings"

import "time"

const testDaemonHiveCommit = "0123456789abcdef0123456789abcdef01234567"

func init() {
	// Production release builds inject the exact source commit through ldflags.
	// Package tests use one stable immutable identity for their temporary binary.
	gitHash = testDaemonHiveCommit
}

func testDaemonDigest() string { return strings.Repeat("a", 64) }

func newTestDaemonServiceSpec(stateDir, executable string, interval time.Duration) (daemonServiceSpec, error) {
	return newDaemonServiceSpecWithIdentity(stateDir, executable, interval, testDaemonHiveCommit, testDaemonDigest())
}

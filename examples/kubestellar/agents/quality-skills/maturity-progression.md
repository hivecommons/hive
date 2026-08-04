# ACMM Maturity Progression Protocol

Track and advance the project's test maturity through ACMM levels.

## Level Definitions

| Level | Name | Has Tests | Has CI | Coverage Config | TDD Markers | Test Mode |
|-------|------|-----------|--------|----------------|-------------|-----------|
| 1 | Inception | No | No | No | No | suggest |
| 2 | Advisory | Yes | No | No | No | suggest |
| 3 | Quality-Gated | Yes | Yes | Yes | No | gate |
| 4 | Security-Aware | Yes | Yes | Yes | Yes | tdd |

## Advancing from Level 1 to Level 2

- Create first test file with real assertions
- Add test run command to README or CONTRIBUTING.md
- Create `test/` or `*_test.go` directory structure

## Advancing from Level 2 to Level 3

- Add CI workflow that runs tests on every PR
- Add coverage reporting (codecov/coveralls)
- Set initial coverage threshold (start conservative, ratchet up)
- Add coverage badge to README

## Advancing from Level 3 to Level 4

- Add TDD markers to CONTRIBUTING.md or CLAUDE.md
- Enforce "failing test first" in PR review checklist
- Coverage threshold above 90%
- Every bug fix includes a regression test

## Progress Tracking

Record progress in beads after each kick:
- Current coverage percentage
- Test file count delta since last kick
- Current maturity level
- Issues created for missing infrastructure

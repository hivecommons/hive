# ACMM runtime fragments

These Markdown fragments document the operating constraints for each AI Codebase Maturity Model (ACMM) level. Use them as policy references when reviewing or customizing the built-in ACMM packs and lane templates.

| File | Applies to | Purpose |
|------|------------|---------|
| [base.md](base.md) | All levels | Shared ACMM rules and invariants. |
| [l1.md](l1.md) | L1 Inception | Assisted/advisory setup constraints. |
| [l2.md](l2.md) | L2 Advisory | Observe-only constraints plus a COMMENT-only reviewer (review-first entry point). |
| [l3.md](l3.md) | L3 Quality-Gated | Measured quality-lane behavior plus the reviewer on the hold-gated PR queue. |
| [l4.md](l4.md) | L4 Security-Aware | Issues plus selected hold-gated PR behavior; reviewer on the hold-gated PR queue. |
| [l5.md](l5.md) | L5 Semi-Autonomous | Hold-gated PR behavior for trusted lanes. |
| [l6.md](l6.md) | L6 Autonomous | Auto-merge-on-green constraints. |

See [src/docs/acmm-policy-matrix.md](../../src/docs/acmm-policy-matrix.md) for the authoritative lane-by-lane policy matrix used by the built-in packs.

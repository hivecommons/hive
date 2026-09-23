# omp-fixture

Workflow definition for the deterministic OMP workbench stand-in
(`pkg/extwork/omp/fixture`). Two stages: `review` (no artifacts) and `report`
(emits `report.md`, listed in the stage receipt). Stages advance only when the
fixture is ticked through its control endpoint.

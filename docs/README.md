# klusterOne docs

Project-level overview, installation, and a 30-second tour live in the
top-level [README](../README.md). This folder is the deep-dive index.

## Contents

| Topic                                          | Page |
|-----------------------------------------------|------|
| Components & data flow (CLI ↔ controller ↔ CRD) | [architecture.md](./architecture.md) |
| Full `kubectl-nm` reference                    | [cli.md](./cli.md) |
| Reconcile lifecycle deep dive (3-node walkthrough) | [reconcile-flow.md](./reconcile-flow.md) |
| How the Script action runs on the host         | [script-action.md](./script-action.md) |
| Full CRD spec + codegen workflow               | [crd-reference.md](./crd-reference.md) |

## Suggested reading order

1. **[architecture.md](./architecture.md)** — get the high-level picture of
   the three pieces (CLI, controller, action registry) and the two
   invariants that govern the orchestrator.
2. **[reconcile-flow.md](./reconcile-flow.md)** — see exactly what happens
   tick-by-tick for a concrete 3-node run, including the budget math.
3. **[cli.md](./cli.md)** — reference for every flag and workflow once you
   know the underlying model.
4. **[script-action.md](./script-action.md)** and
   **[crd-reference.md](./crd-reference.md)** — pick up as needed when
   writing scripts or evolving the CRD spec.

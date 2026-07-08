# One-shot e2e demo

A true end-to-end demonstration of running a formula to completion in a
**transient, isolated** city — the manual, proven recipe that the in-process
`gc run <formula>` execution slice productizes.

## Run it

```bash
GCBIN=/path/to/gc bash oneshot-e2e-demo.sh
```

Requires `gc`, `jq`, and a working Dolt toolchain (the script bootstraps a
managed Dolt store). Expected tail:

```
5/5 SUCCESS: workflow root <id> closed with gc.outcome=pass
```

## What it does (and why it's safe)

1. `gc init --no-start` — manufactures a Dolt-backed throwaway city that is
   **never registered with the shared machine supervisor**. The script asserts
   the city is absent from `gc cities`.
2. `gc start --controller` — runs the **standalone** controller (its own
   `.gc/controller.lock` + socket), not the supervisor, so it cannot perturb
   other cities on the host.
3. Slings `mol-do-work` (the minimal core formula: one work step) on a
   one-member convoy.
4. `self-close-worker.sh` — a minimal, LLM-free worker subprocess: it polls the
   ready queue for the step bead routed to it and closes it with
   `gc.outcome=pass`. The providerless control-dispatcher advances the molecule
   and `workflow-finalize` closes the root.
5. On exit (success or failure) a `trap` stops the controller, kills the city's
   Dolt server, and reaps the temp directory.

Swap `self-close-worker.sh` for a real LLM agent provider and the orchestration
is unchanged — the deterministic worker just makes the demo hermetic and fast.

## Files

- `oneshot-e2e-demo.sh` — the driver.
- `self-close-worker.sh` — the deterministic worker.

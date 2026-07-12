# DAD implementation progress

Base: bfcf5e538 (graph-substrate/p1-journal-store). SLX+TNK already on base.

## Production surface (tiny)
- [x] plan.go:1710-12 fence delete
- [x] engine.go matchingArm ns-aware view (exact §1.2 form)
- [x] Comments: plan.go 1699-1702, 1815, 1845-46, 2302-08, 2344; engine.go 346-47, 1386-88
- [x] Test flip: dispatch_runarm_plan_test.go:330 (positive)

## Tests
- [x] dad_test.go marquee (inline + other-value) §2.1
- [x] §2.2 subject-at-depth: env-bound / seeded-default(a) / root-not-seeded(b) / bound-""-chain(c)
- [x] §2.2d run-sibling + leaf-sibling subject; §2.2e aggregate invisible; §2.2f member LOUD; §2.2g interp-parts silent
- [x] §2.3 root byte-parity (journal-parity + drop+refold)
- [x] §2.4 deep no-match parity leg (pool≡inline, deep-sibling "")
- [x] §2.5 leaf-arm-at-depth DRIVE (render CONTENT + pool cycle) + exec byte-parity
- [x] §2.6 per-pass re-decision + write-once grown fold
- [x] §2.7 crash/resume at depth
- [x] §2.8 cross-arm skip-cascade at depth
- [x] §2.9 plan pins: deep mint coords + flipped test + charset/synth bans at depth + composed provenance
- [x] §2.10 downstream read at depth
- [x] §2.11 loud unregistered-ns (direct + wrap both drivers)
- [x] fixture dispatch-at-depth.lumen.json + fixture-lowers pin
- [x] dolt e2e (WRITE, do not run)

## Gates
- [x] go vet ./internal/lumen/...
- [x] go test ./internal/lumen/engine/ ./internal/lumen/ir/ -count=1
- [x] -race pass
- [x] go vet -tags integration ./test/integration/
- [x] IgnoredGoFiles []

## Red-team fix-forward (2026-07-12)
- [x] P1 mutant KILLED: TestDispatchAtDepthSubjectEnvBindingErrorPropagates (both drivers).
      Protocol: mutant applied (scopeFor err-block deleted) -> full suite GREEN (unkillable
      confirmed) -> pin added -> RED under mutant (both subtests: "run completed, want the
      deep view-build error") -> mutant reverted -> pin GREEN. No residue (grep MUTANT = 0).
- [x] P4: agg-activated-unsettled comment reworded to claim only what is asserted
      (convergence, single bead, refold; pre-crash prompt = genesis render); the genuine
      post-resume render pin added to TestDispatchAtDepthCrashPreMintResumes
      (effectPrompts(resumed) == ["drain fanout"] — only render happens ON resume).
- [x] Gates re-run: vet 0, unit suite ok (24.7s), -race ok (203s, exit 0),
      lint clean on touched files, IgnoredGoFiles [], integration vet 0.

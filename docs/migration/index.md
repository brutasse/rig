# Migration

Four entry points, one strategy.

| You are using… | Read |
|---|---|
| tools.project (`clj -T:project …`), with or without deps-modules | [Migrating from tools.project](tools-project.md) |
| Leiningen (`project.clj` + `lein`) | [Migrating from Leiningen](leiningen.md) |
| deps-modules (managed dependencies / `merge-deps`) without tools.project | [Migrating from deps-modules](deps-modules.md) |
| plain tools.deps (`deps.edn` + `clj`), single or multi-module | [Onboarding a tools.deps project](plain-tools-deps.md) |

## The common strategy

Whatever you are coming from, the migration is the same five steps, and
**each step is independently shippable**:

1. **Add the lock (additive, zero risk).** `rig lock` on the current repo
   and commit `deps.lock`. Nothing else changes — your manifests are
   already self-contained, and rig reads them as-is. Every build is now
   hash-pinned, and you have a baseline to prove parity against.
2. **Swap the command surface.** Replace the old invocations — Makefile
   targets, CI steps, shell muscle memory — with the rig verbs
   (`clj -T:project test` → `rig test`, …). rig is the command surface;
   a Makefile that only wrapped the old tooling is now unnecessary.
   Verify the locked classpath is byte-identical to what the old tooling
   resolved (`clojure -Spath` is the oracle) and that the test suite is
   green.
3. **Adopt the model.** Add `:rig/modules` and `:rig/deps` to the root,
   rewrite the per-module tooling keys to `:rig/*`, and delete the legacy
   machinery (managed maps, inherit markers, `:project` aliases,
   deploy-config keys) — for legacy workspaces that whole rewrite is one
   `rig migrate` command (`--dry-run` first); plain tools.deps projects add
   the keys by hand. `rig check` now surfaces any drift the old
   machinery was hiding; resolve each finding with an explicit
   `rig update`.
4. **Harden CI.** Replace the workflow steps with the frozen gate:
   `rig verify --frozen && rig check --frozen && rig test
   --frozen`, `rig build --uber --frozen`, and `rig release`.
5. **Clean out.** Delete the Makefile targets that only wrapped the old
   tooling and drop the old tools from the toolchain entirely.

Step 1 alone delivers the security core — full-tree pinning, hashing,
offline builds — without changing a single line of existing
configuration. If you stop there, you have already won the important
half.

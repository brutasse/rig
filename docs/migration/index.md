# Migration

Four entry points, one strategy.

| You are using… | Read |
|---|---|
| tools.project (`clj -T:project …`), with or without deps-modules | [Migrating from tools.project](tools-project.md) |
| Leiningen (`project.clj` + `lein`) | [Migrating from Leiningen](leiningen.md) |
| deps-modules (managed dependencies / `merge-deps`) without tools.project | [Migrating from deps-modules](deps-modules.md) |
| plain tools.deps (`deps.edn` + `clj`), single or multi-module | [Onboarding a tools.deps project](plain-tools-deps.md) |

## The common strategy

Whatever you start from, the migration follows the same five steps.
**Each step is independently shippable**:

1. **Add the lock (additive, zero risk).** Run `rig lock` on the current
   repo and commit `deps.lock`. Nothing else changes. Your manifests are
   already self-contained, and Rig reads them as-is. Rig hash-pins every
   build, and you get a baseline to prove parity against.
2. **Swap the command surface.** Replace the old invocations — Makefile
   targets, CI steps, and your command-line habits — with the Rig verbs
   (`clj -T:project test` → `rig test`). Rig is the command surface.
   A Makefile that only wrapped the old tooling is now unnecessary.
   Verify the locked classpath is byte-identical to what the old tooling
   resolved (`clojure -Spath` is the oracle). Verify that the test suite
   passes.
3. **Adopt the model.** Add `:rig/modules` to the root. Add `:rig/deps`
   too, if you want to keep shared requirements. Rewrite the tooling
   keys of each module to `:rig/*`. Delete the legacy machinery: managed
   maps, inherit markers, `:project` aliases, and deploy-config keys.
   Rig materializes the managed maps into the modules; it does not keep
   them as shared requirements. For a legacy workspace, one `rig migrate`
   command does that rewrite (run `--dry-run` first). A plain tools.deps
   project adds the keys by hand. The old machinery hid drift; `rig
   check` now surfaces it. Resolve each finding with an explicit
   `rig update`.
4. **Harden CI.** Replace the workflow steps with the frozen gate:
   `rig verify --frozen && rig check --frozen && rig test --frozen`, `rig build --uber --frozen`, and `rig publish` + tag.
5. **Clean out.** Delete the Makefile targets that only wrapped the old
   tooling and drop the old tools from the toolchain entirely.

Step 1 alone delivers the security core: full-tree pinning, hashing, and
offline builds. It does not change one line of existing configuration.
If you stop there, you already have the important part.

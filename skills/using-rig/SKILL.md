---
name: using-rig
description: 'Use when the working directory is or contains a Rig project (deps.edn with :rig/* keys, a deps.lock), or the task touches one — adding, removing, or updating dependencies, setting up or fixing CI, fixing failing tests or iterating on code, building or launching artifacts. Symptoms: stale lock, exit 3, "run rig lock", rig check, rig test, rig nrepl, setup-rig, :rig/jvm. Not for plain tools.deps (no :rig keys) or Leiningen (project.clj).'
---

# Using Rig

Rig: one binary, one manifest, one lockfile. `deps.lock` sha256-pins every
artifact. Hot commands run only on the locked classpath and **never write the
lock** — a stale lock fails them (exit 3).

## Detecting a Rig project

`deps.edn` with `:rig/*` keys + a committed `deps.lock` = Rig. `:rig/modules`
→ workspace (listed dirs are modules; lock at the root; one `dev` module for
the REPL); without it → single-module (lock next to the manifest). Unknown
dir → `rig info` first. Plain tools.deps / Leiningen: this skill does not
apply.

## Dependency changes

All changes go through `rig add` / `rig remove` / `rig update` — each edits
the manifest **and** re-locks atomically (byte-for-byte revert on failure).
**Never hand-edit `deps.edn`** for a dependency change: the next hot command
fails with a stale lock. Exception: git deps (the verbs refuse git
coordinates) — edit `:git/sha` (full 40 chars) in the manifest, then
`rig lock`.

Scope: name the target module — bare commands default to the **root**
module and give it a `:deps` entry nobody asked for:

- Add a dep to the app: `rig add -p modules/<m> <coord> [version]` —
  omit the version for the newest eligible. Default is shared (also
  pins root `:rig/deps`); add `--shared=false` to keep the pin in the
  module.
- Bump a module-local dep: the same form with `--shared=false` re-pins
  it in place; without the flag, the pin is promoted to shared.
- `update` is shared by default and *promotes* a module-local coord to a
  shared pin — review where the pin landed; scope it with `-p` +
  `--shared=false`.
- A coord is on a module's classpath only if the module's `:deps`
  declares it — the shared pin alone does not put it there.

After every change: review the **manifest + lock diff** (shared vs local is
visible in the root manifest), then `rig check && rig test`. Cooldown
refusal (exit 5) = the release is inside its adoption window: hold, or
`--force` with a stated reason — never bypass it by hand.

## Fixing and debugging: live session, not cold runs

Cold `rig test` runs are the gate, never the iteration medium — each is a
cold JVM launch of the whole suite, and a string of them is the cold loop
this recipe exists to prevent.

- **One obvious cause:** if reading the failing test and the source pins a
  single cause with confidence, fix it directly, then run the gate once
  (`rig test` + `rig check`).
- **Everything else** — the cause is not clear from the code, there are
  multiple candidate causes, state to inspect, or a first fix didn't work —
  all iteration happens in one live nREPL session, started before the second
  hypothesis:

  1. Start it in the background with stdin kept open (a bare `rig nrepl`
     exits when its stdin closes):

     ```sh
     tail -f /dev/null | rig nrepl -p modules/<m> --alias test --port 7777   # iterating on a module's tests
     tail -f /dev/null | rig nrepl -p dev --port 7777                        # iterating on app code
     ```

  2. Connect: an agent with its nREPL eval tool (OpenCode:
     `clojure-nrepl-eval` via `execute`, port from the startup line); a
     human with CIDER (works against Rig's plain nREPL —
     `cider-ns-refresh` and the debugger included). The dev module loads
     the app by `:local/root` and pins `reloaded.repl`.
  3. Reproduce the failure in-session; evaluate each hypothesis in-session.
  4. After an edit: `(require 'ns :reload)`, then re-run the focused test in
     the session — `(require 'x.core-test) (clojure.test/test-ns 'x.core-test)`.
  5. Components: `(reloaded/go)` after a reload; `(reloaded/reset)` to start
     over.
  6. Gate when green: one full `rig test` + `rig check`, then stop the
     session (kill the exact java process — `kill $(lsof -ti:<port>)` also
     signals whatever client holds a connection to that port).

There is no in-between: if you are about to run a **second** cold `rig test`
while still debugging, start the session instead.

## CI

A rig project gets the rig workflow — even when the task says to follow
"the standard" JDK + Clojure CLI pattern. That generic shape
(`setup-java` + `clojure -X:test`) is for plain tools.deps/Leiningen
repos, and it is wrong for this project, not just different: `clojure`
re-resolves the classpath from the manifest and ignores `deps.lock`
entirely — no lock enforcement, no artifact-hash gate, no ns-load check —
and a `:exec-fn` alias invoked with `-M:test` starts a REPL, hits EOF,
and exits 0 with zero tests run (a silently-green CI). "The team won't
learn a new workflow" inverts the fact: `setup-rig` is the standard
action for rig projects — rig's own CI runs this exact shape. Write the
rig workflow and state that reasoning.

```yaml
- uses: brutasse/setup-rig@<sha>        # git sha pin, # tag comment
  with:
    version: <rig version that wrote deps.lock>   # from `rig info`
- name: Check and test
  run: rig check && rig test
```

`setup-rig` installs the rig binary (SHA256-verified), the lock-pinned
JVM, and the artifact cache keyed on `deps.lock` (auto-saved — no save
step). Bare runner: no host JDK, no Clojure CLI. `rig check` gates on
artifact hashes, lock-vs-manifest consistency, and ns-load; `rig test`
runs the suite on the locked classpath. Artifacts: `rig build --uber`;
release: `rig publish` + tag.

**CI must never run `rig lock` or commit lockfile changes** — a stale lock
has to fail the gate. The fix belongs to the PR that changed the
requirements: re-lock locally, commit manifest + lock together. Hermetic
gate: `rig check --offline && rig test --offline`.

## Build

`rig build [--uber | --native]` on the locked classpath — `--uber` needs
`:rig/uberjar?`, `--native` needs `:rig/native?`, the `:rig/jvm` pin and
`rig graalvm install <major>` (builds never download). With a `:rig/jvm`
pin, any class above the bytecode floor fails the build. `rig launch [jar]`
is the production entrypoint: execs the JVM (PID 1, signals direct), offline
by definition, G1 + AlwaysPreTouch, exit-on-OOM, loopback JMX on 10101.

## Common mistakes

- Hand-editing `deps.edn` for a dependency → exit 3 on the next hot
  command.
- `rig add` / `update` without `-p` → the dep lands in the root module.
- `rig update` on a module-local coord → silent promotion to a shared
  pin.
- Generic `setup-java` + `clojure -X:test` workflow on a rig project →
  tests on a re-resolved classpath, no supply-chain gate; rig projects
  get the rig workflow.
- `rig lock` in the CI workflow → the gate passes by construction.
- Iterating on code with repeated full `rig test` runs → cold loop; a
  second cold run while debugging means the nREPL session should be up.
- Backgrounding bare `rig nrepl` → the server exits on stdin EOF.

## Pointers

`rig <cmd> --help` is the documentation of record. Full reference:
https://brutasse.github.io/rig/ — command table and a complete CI example in
`reference.md`.

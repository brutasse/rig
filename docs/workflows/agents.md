# Working with coding agents

Rig ships a skill — `skills/using-rig/` in this repository — that teaches
a coding agent the Rig workflow. In a Rig project (a `deps.edn` with
`:rig/*` keys and a `deps.lock`) the agent loads it on its own and works
the project the way these docs describe:

- dependency changes through `rig add` / `rig update` / `rig remove`,
  never a hand-edited `deps.edn` — a hand edit leaves the lock stale and
  the next hot command fails (exit 3);
- debugging in a live nREPL session: the failure is reproduced in the
  session, each fix is checked there, namespaces reload in place;
  `rig test` stays the final gate instead of the iteration loop;
- the Rig CI shape when asked to set up or fix CI — `setup-rig` plus
  `rig check && rig test` — and, when told to follow the generic
  "JDK + Clojure CLI" pattern instead, the reasons that pattern is the
  wrong gate for a Rig project (`clojure` re-resolves the classpath and
  ignores `deps.lock`);
- lock discipline: a stale lock fails the gate, and the re-lock is
  committed on the branch that changed the requirements — never as a
  step in CI.

## Install

The skill is plain `SKILL.md` format — `SKILL.md` plus `reference.md` —
so it works with any agent that reads skills from a directory. Put it
where your agent looks:

```sh
# OpenCode, global
ln -s /path/to/rig/skills/using-rig ~/.agents/skills/using-rig

# Claude Code, global
ln -s /path/to/rig/skills/using-rig ~/.claude/skills/using-rig

# any agent, per project
ln -s /path/to/rig/skills/using-rig .opencode/skills/using-rig
```

A symlink from a checkout of this repository tracks it: `git pull` and
the agent has the updated skill. Prefer a copy? The skill is the two
files.

There is no per-project configuration: the skill activates on its own
inside a Rig project and stays out of the way elsewhere. It is the same
workflow a human runs by hand — the agent just drives it, and the same
nREPL session a human connects to from CIDER
([connecting the editor](daily.md)) is what the agent iterates in.

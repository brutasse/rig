# Workspaces and modules

A rig project is either a **single module** or a **workspace** of modules.
The difference is one key in the root `deps.edn`.

## Single module

A directory with a `deps.edn` and no `:rig/modules` key is a single-module
project. The directory is the module; `deps.lock` is written next to the
manifest. This is the shape of most libraries and services, and it is
what `rig new` scaffolds (as a one-module workspace, see below).

## Workspace

A `deps.edn` containing `:rig/modules` declares a workspace:

```edn
{:rig/modules ["modules/lib" "modules/app" "modules/schemas"]
 :rig/deps {org.clojure/clojure        {:mvn/version "1.11.0"}
            org.clojure/tools.logging  {:mvn/version "1.1.0"}
            org.clojure/test.check    {:mvn/version "1.1.1"}}
 :rig/version-file "VERSION"}
```

- Each listed directory is a **module**: it has its own `deps.edn`, its
  own source, its own `:rig/lib` coordinate.
- The root `deps.edn` additionally acts as the **workspace manifest**:
  shared requirements under `:rig/deps`, cooldowns, the version file.
- The root is also a module (`"."`) when it has `:deps`/`:paths` of its
  own — a facade for a root REPL or a top-level library.

The workspace root is found by walking up from your current directory:
the nearest `deps.lock`, or failing that the nearest `deps.edn` containing
`:rig/modules`. So `rig test` works from any subdirectory, exactly like
the Clojure CLI finds its project.

## `:rig/deps`: the single place to bump a shared version

`:rig/deps` holds **requirements** for coordinates shared across modules
— usually the same exact version string that appears in the modules' own
`:deps`. It is the canonical, single place to change a cross-module
version:

```sh
rig update com.example/shared 1.0.4
```

sets the requirement in `:rig/deps` **and** in the `:deps` of every module
that declares that coordinate (format-preserving, one visible diff per
file), then re-locks. `rig check` polices the relationship: if a module
requirement and the workspace requirement disagree, you get a `drift`
warning; if the lock no longer satisfies a requirement, you get a
`stale-lock` error.

Values in `:rig/deps` are plain requirement maps — `{:mvn/version …}`,
plus `:exclusions`, `:local/root`, `:git` when needed — not a new
vocabulary.

## Local module dependencies

Modules depend on each other the standard tools.deps way:

```edn
:deps {com.example/schemas {:local/root "../schemas"}
       com.example/lib     {:local/root "../lib"}}
```

In the lock, a local dependency expands to the depended module's source
and resource directories (relative to the workspace root). The build order
follows the dependency graph: `rig test` runs modules in lock order, and a
module's classpath always contains its local dependencies' code.

## `rig new-module`

Add a module to an existing workspace:

```sh
rig new-module reporting
```

This scaffolds `modules/reporting/` (manifest, `src`, `test`), inserts
`"modules/reporting"` into the root's `:rig/modules`, and re-locks — the
new module is part of the workspace from the first `rig test`.

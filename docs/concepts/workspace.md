# Workspaces and modules

A Rig project is either a **single module** or a **workspace** of modules.
The difference is one key in the root `deps.edn`.

## Single module

A directory with a `deps.edn` and no `:rig/modules` key is a single-module
project. The directory is the module, and `deps.lock` lives next to the
manifest. Most libraries and services take this form. `rig new` also
scaffolds this form, as a one-module workspace (see below).

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
  own source, and its own `:rig/lib` coordinate.
- The root `deps.edn` additionally acts as the **workspace manifest**. It
  holds shared requirements under `:rig/deps`, the cooldowns, and the
  version file.
- The root is also a module (`"."`) when it has `:deps` and `:paths` of
  its own. This serves a root REPL or a top-level library.

Rig finds the workspace root by walking up from your current directory. It
looks for the nearest `deps.lock`. If it finds none, it looks for the
nearest `deps.edn` containing `:rig/modules`. So `rig test` works from any
subdirectory, exactly as the Clojure CLI finds its project.

## `:rig/deps`: the single place to bump a shared version

`:rig/deps` holds the **requirements** for coordinates shared across
modules. It usually names the same version string that the `:deps` of the
modules themselves use. It is the single place to change a cross-module
version:

```sh
rig update com.example/shared 1.0.4
```

This command sets the requirement in `:rig/deps` **and** in the `:deps` of
every module that declares that coordinate. It preserves the file format
and produces one visible diff per file. Then it re-locks. `rig check`
checks this relationship: if a module requirement and the workspace
requirement disagree, Rig reports a `drift` warning. If the lockfile no
longer satisfies a requirement, Rig reports a `stale-lock` error.

Values in `:rig/deps` are plain requirement maps: `{:mvn/version …}`, plus
`:exclusions`, `:local/root`, and `:git` when you need them. This is not a
new vocabulary.

## Local module dependencies

Modules depend on each other the standard tools.deps way:

```edn
:deps {com.example/schemas {:local/root "../schemas"}
       com.example/lib     {:local/root "../lib"}}
```

In the lockfile, a local dependency expands to the source and resource
directories of the depended module (relative to the workspace root). The
build order follows the dependency graph: `rig test` runs modules in lock
order. The classpath of each module always contains the code of its local
dependencies.

Rig also locks a `:local/root` reference to a directory that is **not** a
workspace module as a local module. One example is a `dev/` test overlay
with its own `deps.edn`. The entry expands to the manifest paths of that
directory itself. The directory stays out of `:rig/modules`, so it is
never a build, test, or publish target.

## `rig new-module`

Add a module to an existing workspace:

```sh
rig new-module reporting
```

This scaffolds `modules/reporting/` (manifest, `src`, `test`), inserts
`"modules/reporting"` into the `:rig/modules` of the root, and re-locks.
From the first `rig test` on, the new module is part of the workspace.

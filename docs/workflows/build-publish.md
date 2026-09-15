# Build, publish, release

## Building

```sh
rig build -p modules/orchestrator        # the module jar
rig build --uber -p modules/orchestrator # the uberjar
rig build                                # the root module
```

The jar/uberjar come from the module's `:rig/*` build config (`:rig/uberjar?`,
`:rig/uberjar-file`, `:rig/uber-opts`, …) applied to the **locked**
classpath — no re-resolution. Output is one line:

```
built /…/modules/orchestrator/target/orchestrator.jar
```

`rig build` builds the module jar on the locked classpath; `--uber` builds
the uberjar and requires `:rig/uberjar?`. A module without an uberjar fails
`--uber` with a usage error.

## Installing locally

```sh
rig install                    # every module with a :rig/lib
rig install -p modules/lib     # one module
```

Builds each target and drops the jar (and a generated POM) into the local
Maven repository — the same layout `mvn install` would produce. Other
projects on the machine that depend on your library by coordinate can
pick it up immediately.

## Publishing

```sh
rig publish                    # every module with :rig/publish?
rig publish -p modules/lib
```

Builds the module and deploys the jar + POM to the remote repository named
in `:rig/publish` (default `{:repo "clojars" :sign-releases? false}`).
Credentials are the basic-auth entries in `~/.m2/settings.xml` matched by
repository id — the same file Maven and tools.deps use, nothing new to
configure.

`publish` always needs the network; `rig publish --offline` is a usage
error, not a silent no-op.

## Releasing

`rig release` performs the full release sequence for the workspace in one
go:

```
1. remove-snapshot: 1.2.3-snapshot -> 1.2.3 (VERSION)
2. publish: 2 module(s)
3. commit: VERSION ("Release v1.2.3")
4. tag: v1.2.3
5. bump-and-snapshot: 1.2.3 -> 1.2.4-snapshot (VERSION)
6. commit: "Bump version to 1.2.4-snapshot"
7. push: origin main + v1.2.3
```

Concretely:

1. strip `-snapshot` from the `VERSION` file (and re-lock, since the
   version is part of the lock);
2. publish every `:rig/publish?` module at the release version;
3. commit the `VERSION` change;
4. tag `v<version>`;
5. bump the patch and re-add `-snapshot`;
6. commit `VERSION` + the refreshed `deps.lock`;
7. push the branch and the tag.

```sh
rig release --dry-run          # print the plan, change nothing
```

The dry run is the way to sanity-check what a release will do — it shows
the exact version transitions and which modules would be published where,
and notes if the lock would first need re-locking.

Versioning conventions:

- The `VERSION` file holds `X.Y.Z` or `X.Y.Z-snapshot`; `release` works on
  the file, not on a config key.
- Per-module versions come from the module's `:rig/version-file` (a path
  to the file — commonly `../../VERSION` in multi-module setups) or the
  workspace root's.
- An existing tag makes the release fail rather than overwrite.

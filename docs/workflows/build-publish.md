# Build, publish, release

## Building

```sh
rig build -p modules/orchestrator        # the module jar
rig build --uber -p modules/orchestrator # the uberjar
rig build --native -p modules/app        # the native-image binary
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

Java: a module's `:rig/java-src-dirs` are javac'd before its Clojure is
compiled, and local dependency modules that declare them are javac'd
automatically before the consumer builds — see
[Java sources](../concepts/java.md).

`--native` builds a GraalVM native-image binary instead of a jar: the
module declares `:rig/native?`, the workspace pins `:rig/jvm` (the
GraalVM major is derived from the pin). The build never downloads:
`rig build --native` fails with a hint when no GraalVM for the major is
installed — install it with `rig graalvm install <major>` first. The
binary is standalone — no JVM, no classpath — and runs on plain args
with `:rig/main` as entry namespace
(see [Native images](../reference/config.md#native-images-rig-build-native)).
A native build needs a C compiler and a few GB of RAM, and takes minutes.

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

A release is a version commit plus a publish and a tag: Rig does the
publish; the version history and the tag are yours.

```sh
# 1. the release version — commit it (the version is part of the lock)
echo 1.2.3 > VERSION && rig lock
git commit -am "Release v1.2.3"

# 2. publish every :rig/publish? module at 1.2.3
rig publish

# 3. tag the release and push it
git tag v1.2.3 && git push origin --tags

# 4. start the next cycle
echo 1.2.4-snapshot > VERSION && rig lock
git commit -am "Bump version to 1.2.4-snapshot"
```

- Do not publish while `VERSION` still holds `-snapshot`: you would deploy
  a snapshot version to your release repository, and Maven repositories do
  not re-publish a version.
- An existing tag makes the push fail rather than overwrite.

Versioning conventions:

- The `VERSION` file holds `X.Y.Z` or `X.Y.Z-snapshot`; the release works
  on the file, not on a config key.
- Per-module versions come from the module's `:rig/version-file` (a path
  to the file — commonly `../../VERSION` in multi-module setups) or the
  workspace root's.

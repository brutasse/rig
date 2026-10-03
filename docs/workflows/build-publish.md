# Build, publish, release

## Building

```sh
rig build -p modules/orchestrator        # the module jar
rig build --uber -p modules/orchestrator # the uberjar
rig build --native -p modules/app        # the native-image binary
rig build                                # the root module
```

The jar and uberjar come from the `:rig/*` build config of the module
(`:rig/uberjar?`, `:rig/uberjar-file`, `:rig/uber-opts`, …). Rig applies
this config to the **locked** classpath. There is no re-resolution. The
output is one line:

```
built /…/modules/orchestrator/target/orchestrator.jar
```

`rig build` builds the module jar on the locked classpath. `--uber` builds
the uberjar and requires `:rig/uberjar?`. A module without an uberjar fails
`--uber` with a usage error.

Java: Rig compiles a module's `:rig/java-src-dirs` with javac before it
compiles that module's Clojure. Rig also compiles local dependency modules
that declare them before the consumer builds — see
[Java sources](../concepts/java.md).

`--native` builds a GraalVM native-image binary instead of a jar. The
module declares `:rig/native?`, and the workspace pins `:rig/jvm`. The pin
determines the GraalVM major. The build never downloads:
`rig build --native` fails with a hint when no GraalVM for that major
exists on the machine. Install one with `rig graalvm install <major>`
first. The binary is standalone — no JVM, no classpath — and runs on plain
args with `:rig/main` as the entry namespace
(see [Native images](../reference/config.md#native-images-rig-build-native)).
A native build needs a C compiler and a few GB of RAM, and takes minutes.

## Installing locally

```sh
rig install                    # every module with a :rig/lib
rig install -p modules/lib     # one module
```

`rig install` builds each target and drops the jar (and a generated POM)
into the local Maven repository. The layout is the same one
`mvn install` produces. Other projects on the machine that depend on your
library by coordinate can pick it up immediately.

## Publishing

```sh
rig publish                    # every module with :rig/publish?
rig publish -p modules/lib
```

`rig publish` builds the module and deploys the jar and POM to the remote
repository named in `:rig/publish` (default
`{:repo "clojars" :sign-releases? false}`). The credentials are the
basic-auth entries in `~/.m2/settings.xml` that match the repository id.
Maven and tools.deps use the same file, so there is nothing new to
configure.

`publish` always needs the network. `rig publish --offline` is a usage
error, not a silent no-op.

## Releasing

A release is a version commit plus a publish and a tag. Rig does the
publish. The version history and the tag are yours.

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

- The `VERSION` file holds `X.Y.Z` or `X.Y.Z-snapshot`. The release process
  uses the file, not a config key.
- The version of each module comes from the `:rig/version-file` of the
  module (a path to the file — commonly `../../VERSION` in multi-module
  setups) or from the version file of the workspace root.

# Configuration

All Rig configuration lives under one namespace — `:rig/*` — in the
`deps.edn` files. Rig does not change the standard tools.deps keys
(`:paths`, `:deps`, `:aliases`); the tools.deps spec documents them. Rig
extends `:mvn/repos` with an `:auth` marker (see
[authenticated repositories](#authenticated-repositories-auth-oidc)).

## Workspace root keys

Set in the root `deps.edn` of a workspace (a `deps.edn` containing
`:rig/modules`).

| Key | Default | Meaning |
|---|---|---|
| `:rig/modules` | — | The workspace's module directories (relative to the root). A `deps.edn` with this key is a workspace. |
| `:rig/deps` | — | Shared requirements: the single place to bump a cross-module version. Values are plain requirement maps (`{:mvn/version …}`, optionally with `:exclusions`, `:local/root`, `:git`). `rig update` keeps this in sync with the modules, and `rig check` reports drift. It is not merged into module classpaths: modules keep declaring their own `:deps`. |
| `:rig/cooldown` | `"48h"` | Minimum age of a version before it may be selected. `"0s"` disables. See [cooldowns](../concepts/security.md#cooldowns-the-adoption-window). |
| `:rig/cooldown-repos` | — | Per-repository cooldown overrides, keyed by `:mvn/repos` id: `{"corp" "72h"}`. |
| `:rig/jvm` | — | The JVM the project runs on, as a major (feature) version: `"21"` (Temurin). Rig manages the exact release — the newest one installed in the state dir matching the major, or the newest release when online (see [JVMs](#jvms-rigjvm)). Minimum supported value: `8` — the kernel jar is built for Java 8, so older JVMs cannot load it. |
| `:rig/compile-jvm-opts` | `[]` | JVM flags for the build/validate JVMs: the AOT build — the kernel JVM and the compile fork it launches — and the check namespace load/AOT-compile (see [JVM flags](#jvm-flags)). Version-sensitive flags like `--enable-preview` require a `:rig/jvm` pin. |
| `:rig/version-file` | `"VERSION"` | Root version file recorded in the lock for the root module. |

The root `deps.edn` can also carry its own `:deps`/`:aliases`/`:paths`.
Rig then resolves the root as a module (`"."`) in its own right.

## Module keys

Set in the `deps.edn` of each module. All keys are optional unless noted.

| Key | Default | Meaning |
|---|---|---|
| `:rig/lib` | — (required to `install`/`publish`) | The module's Maven coordinate, e.g. `com.example/my-lib`. |
| `:rig/version` | from the version file | An explicit version string; overrides the version file. |
| `:rig/version-file` | `"VERSION"` | Path to the version file (module dir, then workspace root) used when the module's version is recorded in the lock. Commonly `"../../VERSION"` in submodules of a shared-version project. |
| `:rig/version-fn` | — | Dynamic version, keyword only (the kernel jar cannot load user code): `:git-count-revs` — the template's `GENERATED_VERSION` marker replaced with the commit count since the repo root; or `:epoch` — the current unix time in seconds. Used when neither `:rig/version` nor a version file is present. The version moves with the repo, so the lock records the snapshot taken at lock time. |
| `:rig/version-template-file` | `"VERSION_TEMPLATE"` | The template file read by `:rig/version-fn :git-count-revs` (module dir, then workspace root). |
| `:rig/main` | — | The namespace `rig run` (and `rig launch`, via the baked launch descriptor) launches (`-main`). Also the entry namespace of a native-image binary. |
| `:rig/uberjar?` | `false` (true if `:rig/uberjar-file` is set) | Build an uberjar (with `rig build --uber`). |
| `:rig/native?` | `false` (true if `:rig/native-file` is set) | Build a GraalVM native-image binary (with `rig build --native`). |
| `:rig/native-file` | `target/<lib name>` | Native binary output path, relative to the module (no extension). |
| `:rig/native-opts` | `[]` | Extra native-image arguments, appended after Rig's fixed ones. |
| `:rig/uberjar-file` | `target/<name>-<version>.jar` | Uberjar output path, relative to the module. |
| `:rig/uber-opts` | `{}` | tools.build uberjar options (currently only `:exclude`), e.g. `{:exclude ["META-INF/license/.*"]}`. Patterns are FULL matches against entry names (`re-matches`): a prefix pattern like `"^META-INF/license/"` excludes nothing — rig warns about patterns that match no entry, and about opts keys it does not consume. |
| `:rig/timestamp-string` | — | ISO-8601 UTC instant (e.g. `"2026-01-01T00:00:00Z"`) used as the timestamp of every entry in the built jar and uberjar; entries are also written in name-sorted order. Set together with a pinned `:rig/jvm`, this makes the build output byte-reproducible. Pre-1980 values are refused (the zip format would clamp them silently). |
| `:rig/test?` | `true` | Recorded in the lock (`test.enabled`). `rig test` itself targets modules by their `:test` alias's `:exec-fn`. |
| `:rig/publish?` | `false` (true if `:rig/publish` is set) | Whether `rig publish` deploys this module. |
| `:rig/publish` | `{:repo "clojars" :sign-releases? false}` | Where to publish. `:repo` is a repository id resolved through `~/.m2/settings.xml` credentials and `:mvn/repos` URLs (http/https repositories only); `:sign-releases?` must be `false` (signing is not supported). |
| `:rig/target-dir` | `"target"` | Build output directory. |
| `:rig/artifact-dirs` | the module's own `:paths` entries (plain, in-module, excluding the `:rig/target-dir` tree), else `["src" "resources"]` | Directories compiled into the artifact (jar/uber/native); their non-source files (resources) are copied alongside the classes. The module's `:paths` are its classpath; artifact-dirs are what the build packages — external paths and build output on the classpath stay out of the artifact. `.clj`/`.cljc` are compiled, not shipped: a source next to its AOT `__init.class` in the jar is a load-time recompile hazard (two class identities, `ClassCastException`). |
| `:rig/java-src-dirs` | `[]` | Java source directories (compiled into the jar). |
| `:rig/javac-opts` | `[]` | javac options (used only when `:rig/java-src-dirs` is non-empty). When the workspace pins `:rig/jvm`, `--release <n>` is prepended unless the opts already set `--release`, `-source` or `-target`. |
| `:deps/prep-lib` | — | The standard tools.deps prep-library key: a custom function that prepares the module (staleness-checked) before `rig build`, `rig test`, `rig run` or `rig repl` — e.g. compiling generated Java with an in-JVM Maven. Shape: `{:ensure "target/classes" :alias :prep :fn build/compile-java}`. See [prep functions](#prep-functions-depsprep-lib). |
| `:jvm-opts` | `[]` | The standard tools.deps key: JVM flags for **dev execution** — `rig run`, `rig repl`, `rig exec`, `rig test` — and the module's prep function. It does not apply to `rig build`, `rig check` or `rig launch` (see [JVM flags](#jvm-flags)). |
| `:rig/launch-opts` | `[]` | JVM flags for **`rig launch`** (the production entrypoint); baked into the artifact's launch descriptor at build time (see [JVM flags](#jvm-flags)). |
| `:rig/ns-compile` | — | Extra namespaces to AOT-compile alongside the module's own sources, e.g. `[:entry.main]`. |

## Test alias convention

`rig test` runs what the `:test` alias of the module declares — the standard
tools.deps `:exec-fn`:

```edn
:aliases
{:test {:extra-deps {lambdaisland/kaocha {:mvn/version "1.66.1034"}}
        :extra-paths ["test"]
        :exec-fn kaocha.runner/exec-fn
        :jvm-opts ["-Dlogback.configurationFile=test.logback.xml"]
        :env {"DB_URL" "localhost:5432"}}}
```

Any `exec-fn` from a runner works (kaocha, clojure.test via a small fn, …).
The commands `rig test`, `rig run --alias`, and `rig repl --alias` apply
`:jvm-opts` and `:env` from the alias, exactly as the Clojure CLI applies
them.

Other aliases (`:dev`, …) are first-class for `rig run --alias`, `rig repl
--alias`, `rig exec --alias`, and `rig tree --alias`. Rig resolves them
into the lock like `:test`.

## Prep functions (`:deps/prep-lib`)

Rig honors this standard tools.deps prep-library key: the function
prepares a module before anything builds or runs it. Use it for code
generation that must run inside the classpath of the module, or for
AOT-compiling the Clojure code of the module for its dependents. You can
also compile generated Java with an in-JVM Maven:

```edn
;; a/proto/deps.edn
{:deps/prep-lib {:ensure "target/classes" :alias :prep :fn build/compile-java}
 :aliases       {:prep {:deps {io.github.clojure/tools.build
                               {:git/tag "v0.8.2" :git/sha "ba1a2bf"}}
                        :extra-paths ["build"]
                        :ns-default build}}}
```

- `:ensure` — a path or paths (relative to the module) the function
  guarantees. Rig re-runs the function when one is absent. The clean
  step of the build never deletes them: it wipes only the previous
  output of the build. A class dir the prep owns therefore survives
  `rig build` of the module itself and lands in the jar.
- `:alias` — an alias of the module. The function runs on the **locked**
  classpath of the alias. Rig performs no re-resolution and passes the
  function no repository URLs. The function can still do its own in-JVM
  work, for example a `b/create-basis` against `~/.m2`. The alias
  typically declares the build file under `:extra-paths` and the
  namespace of the function under `:ns-default`.
- `:fn` — a var resolved on the prep classpath, called with a single
  `nil` argument. This is the way tools.deps invokes `exec-prep!` when
  the alias declares no `:exec-args`, so declare it `[f]` or `[& _]`,
  not `[]`. The exit status of the child propagates: a failing prep
  fails the command.

`rig build`, `rig test`, `rig run` and `rig repl` prepare the target
module and every local module on its classpath that declares a prep (or
`:rig/java-src-dirs`), in dependency order. The function re-runs when a
`:ensure` path is absent, when its stamp is out of date, or when the
same invocation re-prepped a local dependency. The stamp covers the
manifest hash, a content digest of the sources of the module, a digest
of the locked dependencies, and the function name. A fresh run records
its digests under `target/.rig-prep.json`; `rig clean` removes the
output and the stamp together. The JVM of the function gets the
`:jvm-opts` of the module and of the prep alias, and runs from the
directory of the module.

If the function is only "javac my own sources into my class dir", Rig
does that natively: declare `:rig/java-src-dirs` and drop the prep
library ([Java sources](../concepts/java.md#migrating-away-from-depsprep-lib)).

## Authenticated repositories (`:auth :oidc`)

Rig extends `:mvn/repos` (root or module manifest) with an `:auth` marker.
`:auth :oidc` marks the repo as behind an OIDC gate:

```edn
:mvn/repos {"corp" {:url "https://maven.corp.example" :auth :oidc}}
```

Every request Rig makes to a marked repo — kernel repo probes, `rig
publish` uploads, and the artifact traffic of the resolver (see the auth
proxy below) — carries `Authorization: Bearer <token>`.

### OIDC gates

An OIDC gate is an identity provider and the audience and client its
tokens must carry, plus the repository URL prefixes it fronts. A whole
organization shares its gates, so they live in one config file, not in
any `deps.edn`:

```yaml
# ~/.config/rig/auth.yaml
gates:
  pier:
    url: https://pier.example          # repo URL prefixes this gate fronts
    well-known: https://idp.example/.well-known/openid-configuration
    audience: pier                     # default: "pier"
    client-id: rig                     # default: "rig"
    # redirect-uri: http://127.0.0.1:8080/callback
    # (browser flow: a fixed loopback URI, when the IdP requires one)
```

Fields:

| Field | Required | Meaning |
|---|---|---|
| `url` | no | A repo URL prefix, or a list of them. A gate without `url` fronts every `:auth :oidc` repo that no other gate fronts — at most one gate may omit it. |
| `well-known` | yes | The issuer's OpenID discovery URL. |
| `audience` | no | The audience the token must carry (default `pier`). |
| `client-id` | no | The OAuth client identifier (default `rig`). |
| `redirect-uri` | no | A fixed `http://127.0.0.1:PORT/callback` redirect for the browser flow. Without it Rig listens on an ephemeral loopback port. |

Rig binds a marked repo to the gate that fronts its `:url`
(longest-prefix match, on the URL). When exactly one gate matches, Rig
uses it. When several gates match, the command fails fast ("tighten the
gate urls"). When no gate matches by prefix, Rig falls back to the single
url-less gate. When there is no match at all, the command fails fast,
before any resolution work, and names the repo, the config file, and the
missing prefix.

### Resolving a gate's token

Rig resolves the bearer of each gate once per command, per repo:

1. `RIG_TOKEN_<GATE>` when set — Rig uppercases the gate name and turns
   dashes into underscores (`pier` → `RIG_TOKEN_PIER`). This is the CI
   path: the runner injects the token.
2. Else the cached token of the gate in the state dir
   (`~/.local/share/rig/oidc/`, one file per gate, valid until 30s before
   its expiry).
3. Else a fresh negotiation with the issuer of the gate. Rig uses the
   browser flow (authorization code + PKCE, opened in your browser) when
   a browser is available. Otherwise Rig uses the device-code flow
   (RFC 8628) in headless environments. Rig verifies the issued token
   against the JWKS of the issuer (signature, issuer, audience, expiry)
   before it uses or caches it.

`rig auth get [gate|url]` prints the token of one gate to stdout, using
the same chain:

```console
$ rig auth get pier          # by gate name
$ rig auth get https://maven.corp.example   # or by a repo URL it fronts
eyJhbGciOi...
```

With no argument, the config must hold exactly one gate. `--flow
browser|device` forces the negotiation flow (auto by default).

### Auth proxy

tools.deps (MIMA) cannot carry a bearer of its own, so Rig does not hand
it the real repo URL. For every resolution run, Rig starts a local
loopback proxy and rewrites the `:url` of each marked repo to it. The
proxy forwards every Maven request — exact versions and floating ones
alike — to the real URL of the repo, with the bearer of that repo
attached. It streams the response without touching disk. The tokens never
leave the process memory of Rig, and the proxy dies with the command.
The repo id stays the same, so lock attribution (and the Maven
`_remote.repositories`) still point at the original repo, and the lock
records its original URL. The kernel probes floating versions against the
original URL, with the per-repo bearer and subject to the cooldown. The
artifacts themselves always come through the proxy.

!!! tip "One repository for everything"
    Redeclaring `central` under `:mvn/repos` routes every Maven artifact
    through a single Pier-backed repository — the resolver probes `central`
    first and Pier serves everything. Redeclare `clojars` too to also cut
    the built-in real Clojars fallback.
    [All artifacts through one repository](../workflows/pier.md).

## JVMs (`:rig/jvm`)

`:rig/jvm` pins the JVM the project runs on:

```edn
{:rig/jvm "21"}
```

- The pin is the major (feature) version of the project — what matters
  for compatibility. `rig lock` records it in `deps.lock` as-is. Rig
  manages the exact release: the newest one installed in the state dir
  that matches the major, or the newest matching release when Rig must
  fetch one.
- When a command needs a JVM, `rig` first uses the rig-managed JDK
  matching the pin when one is present in the state dir. It takes
  precedence over the system `java`, even when the feature version of the
  system matches. Only when no managed JDK exists does a matching system
  JDK (`JAVA_HOME`, then `PATH`) serve. When neither is present and Rig
  is online, Rig downloads, sha256-verifies and installs the newest
  matching release into the state dir on demand (one visible line). Under
  `--offline` this fails with a hint.
- Rig keeps one JDK per major version. `rig jvm install <major>`
  installs the newest release for it, and exits without changes when a
  JDK for the major already exists. `rig jvm update` moves the installed
  JDK for the pinned major to the newest release, and replaces it.
- Managed JDKs live in the state dir
  (`~/.local/share/rig/jdks/temurin-<version>/`); `rig jvm list` shows them
  alongside the system `java`. Every JVM Rig launches to run your code
  gets `JAVA_HOME` set to the managed JDK. The native-image build is the
  exception: Rig runs it on the managed GraalVM.
- Projects that `rig new` scaffolds start with the current LTS pin (Rig
  omits it under `--offline`, or when the Adoptium lookup fails).
- Without the pin, `rig` uses `JAVA_HOME`, then `java` on `PATH`. When
  neither is available, the error suggests installing the current LTS.
  Rig looks the LTS feature up from the Adoptium info endpoint, but Rig
  installs nothing automatically. `RIG_JAVA=<path>` overrides everything
  (dev override, like `RIG_KERNEL_JAR`).
- Rig compiles the module's Clojure code on the JVM it uses for the
  workspace. `rig build` runs that compile in a second process, on the
  same JVM as the kernel.
- The pin is also the **bytecode floor** of the build output of the
  module. Every class in the jar or uberjar `rig build` produces
  (dependency classes included, for the uberjar) must load on the pinned
  JVM. The build fails and lists the offending entries when one does not
  load. Rig checks a multi-release jar entry only at the JVM versions
  where it can load. Without the pin there is no floor and no check.

Vendor: Temurin (Eclipse Adoptium), GA releases only.

## JVM flags

Three contexts, three keys — every JVM Rig launches gets its flags from
the key that matches how Rig uses the JVM:

| Key | Declared in | Applies to |
|---|---|---|
| `:rig/compile-jvm-opts` | workspace (root manifest) | The AOT build — kernel JVM and the compile fork it launches (`rig build`) — and the check namespace load/AOT-compile (`rig check`, stage 2). |
| `:jvm-opts` | module (standard tools.deps key) | Dev execution: `rig run`, `rig repl`, `rig exec`, `rig test`, plus the module's prep function. |
| `:rig/launch-opts` | module | `rig launch` only. Baked into the artifact's launch descriptor at build time, so it travels with the jar. |

The `rig build` JVMs (kernel and compile fork), `rig check` and `rig
launch` never read `:jvm-opts`. The exception is the prep function of
the module, which runs as part of a build and takes `:jvm-opts` like any
dev execution. Dev execution never reads `:rig/launch-opts`. The stage-1
metadata kernel op of `rig check` runs bare — it inspects the lock, not
the code, so it needs no workspace flags.

The canonical case is `--enable-preview`: preview features are
JVM-version-specific, so pin the JVM and set the flag in each context that
compiles, loads or runs preview code:

```edn
;; root deps.edn
{:rig/jvm "21"
 :rig/compile-jvm-opts ["--enable-preview"]}

;; module deps.edn
{:rig/launch-opts ["--enable-preview"]
 :jvm-opts ["--enable-preview"]}
```

Rig rejects a lock at load time when it carries `--enable-preview` in
`:rig/compile-jvm-opts` but no `:rig/jvm` pin: the preview set depends
on the JVM version, and Rig will not guess it.

## Native images (`rig build --native`)

`:rig/native?` declares that a module builds a standalone native-image
binary (GraalVM):

```edn
{:rig/native? true :rig/main app.core}
```

- **GraalVM.** Rig derives the version from the `:rig/jvm` pin of the
  workspace — `--native` without a pin is a usage error — and records it
  in the lock (`graalvm: {vendor, requested}`). The build never
  downloads: when no GraalVM for the major exists, `rig build --native`
  fails with a hint — install it first with
  `rig graalvm install <major>`. The install downloads from the
  `graalvm/graalvm-ce-builds` GitHub releases, verifies the sha256, and
  installs into the state dir `~/.local/share/rig/graal/`. Rig keeps one
  GraalVM per major; manage them with `rig graalvm` (install / list /
  uninstall / update).
  `RIG_GRAALVM_HOME=<home>` overrides the store (a dev override, like
  `RIG_JAVA`; it must contain `bin/native-image`).
  GitHub limits unauthenticated release lookups to 60 requests per hour
  per IP address — shared CI runners exhaust that pool. Rig authenticates
  the lookup with `GH_TOKEN` or `GITHUB_TOKEN` when you set one; GitHub
  Actions exports `GITHUB_TOKEN` for every step.
- **Entry point.** `:rig/main` must be a Clojure namespace. Rig compiles
  a small entry shim (javac beside the java of the workspace) whose main
  delegates to `clojure.main` with `-m <ns>`, so the binary runs with
  plain args: `<binary> arg1 arg2`.
- **Class initialization.** A native image cannot load classes from a
  classpath at run time. Rig loads the Clojure runtime and the namespace
  of the module at image build time, and marks every namespace package it
  finds on the classpath for build-time initialization. When the code
  reaches a namespace only dynamically (outside the transitive `require`
  closure of `:rig/main`), the module must `require` it at top level.
  Otherwise the build fails and names the missing class.
- **Arguments.** Rig passes the fixed arguments `--no-fallback`,
  `--class-path <class dir>:<locked classpath>`,
  `--initialize-at-build-time=<namespace packages>`, and
  `-o <native-file>`; Rig appends `:rig/native-opts` after them. A
  repeated `--initialize-at-build-time` in the opts adds to the list of
  Rig; `--fallback` conflicts with the `--no-fallback` of Rig.
- **Prerequisites.** A full JDK (Rig compiles the shim with `javac`), a
  C compiler for linking, and a few GB of RAM. A native build takes
  minutes, not seconds.
- **Reflection.** Rig does not manage native-image configuration in v1:
  native-image auto-discovers the `META-INF/native-image/**` entries in
  dependency jars. You pass extra configuration through
  `:rig/native-opts` (`--features=…`, `--initialize-at-build-time=…`,
  …).

## Production launch (`rig launch`)

`rig launch` is the production entrypoint (see [Production launch](../workflows/production-launch.md)
for the workflow: launch opts, the runtime image, the entrypoint). It
replaces its own process with the JVM (process exec): the app is the
process itself, in a container its PID 1. Signals reach the JVM directly,
and the exit code of the JVM is the exit code of the process (on Windows
rig forks and waits). Rig then runs the artifact with its production JVM
flag set. Rig applies the flags in a fixed order:

1. **The defaults of Rig** — the launching rig applies them at launch
   time (Rig does not bake them into the artifact, so the policy tracks
   rig upgrades):

   | flag | purpose |
   |---|---|
   | `-XX:+UseG1GC` | the default garbage collector — balanced pauses and throughput |
   | `-XX:+AlwaysPreTouch` | commit every heap page at startup, so the app never pays first-touch page faults under load |
   | `-XX:+ExitOnOutOfMemoryError` | fail fast on OOM instead of limping along — the orchestrator restarts |
   | `-XX:+HeapDumpOnOutOfMemoryError` | leave a heap dump for the post-mortem when that OOM happens |
   | `-Dcom.sun.management.jmxremote` plus the `jmxremote` properties (`port=10101`, `rmi.port=10101`, `authenticate=false`, `ssl=false`) and `-Djava.rmi.server.hostname=127.0.0.1` | loopback-only JMX on the fixed port **10101**, no auth, no SSL — same-host monitoring only |

2. **The `:rig/launch-opts` of the module** — later flags override the
   defaults of Rig (last JVM flag wins; a repeated `-D` re-sets the
   property). Rig reads them from the module manifest, or from the baked
   launch descriptor when you launch a jar standalone. A garbage
   collector in `:rig/launch-opts`, for example `-XX:+UseZGC`, *replaces*
   the G1 default: the JVM refuses to start with two collectors, so Rig
   drops its own rather than pass both.
3. **`RIG_LAUNCH_OPTS` (env var)** — per-deployment JVM flags,
   whitespace-separated, appended last: the same rules apply — last JVM
   flag wins. A collector in it replaces any earlier selection, including
   one from `:rig/launch-opts`. This is the way to tune a deployment
   (heap size, collector, properties) without rebuilding the artifact.

### What happens when a later stage overrides

Rig concatenates all three stages, in order, and passes them to the JVM
as one flag list. Two mechanisms then apply:

- **the last-wins rule of the JVM** — for every flag the JVM uses the
  last occurrence: a later `-Xmx2g` beats an earlier `-Xmx1g`, and a
  repeated `-Dfoo=…` re-sets the property. Boolean flags are idempotent.
  Rig still passes the earlier occurrence; the JVM just ignores it.
- **the collector rule of rig** — the one flag family rig handles
  itself: selecting two collectors at once is a fatal VM error. So rig
  drops every collector selection but the *last* across all three
  stages. Only one collector ever reaches the JVM.

| collector declared in | survives | dropped by rig |
|---|---|---|
| nowhere | the G1 default | — |
| `:rig/launch-opts` (e.g. `-XX:+UseZGC`) | that collector | the G1 default |
| `RIG_LAUNCH_OPTS` (e.g. `-XX:+UseSerialGC`) | that collector | the G1 default **and** the launch-opts collector |

The negated form counts as a selection (`-XX:-UseG1GC` triggers the same
rule), and rig passes the same collector from two stages only once.

To change a default: to move the JMX port, override *both*
`-Dcom.sun.management.jmxremote.port=…` and
`-Dcom.sun.management.jmxremote.rmi.port=…`. To reach JMX from outside
the container, also override `-Djava.rmi.server.hostname=…` and the
`authenticate`/`ssl` flags. To switch a default off, pass its negation
(for example `-XX:-AlwaysPreTouch`).

### The launch descriptor

`rig build` bakes the launch plan of the artifact into every jar and
uberjar as `META-INF/rig/launch.json`:

```json
{"version": 1, "rig": "0.4.0", "main": "app.core",
 "jvm-opts": ["-Xmx2g"], "java": 21, "uber": true}
```

The descriptor records `main`, `jvm-opts`, `java` (the feature version
of the build JVM) and `uber`; Rig takes all of them from the lock at
build time. The JSON field `jvm-opts` holds the value of the module key
`:rig/launch-opts` — the field name predates the key and stays, so old
artifacts keep launching. The production defaults of Rig are *not* in
the descriptor: the launching Rig applies them, so the policy follows
Rig upgrades even for old artifacts. `rig launch <jar>` reads the
descriptor first, so a built jar launches standalone, without the
workspace.

### Behavior

- **Jar selection.** The first positional is the jar when it names an
  existing file. Otherwise (in a workspace) the jar is the build output
  of the target module from the lock (the uberjar when the module
  declares one). All positionals go to the main.
- **Non-uber jars** launch with the locked classpath, where the built
  jar replaces the source paths of the module — which is why they only
  launch inside the workspace.
- **Java version.** The major version of the launch JVM must *exactly*
  match the `java` field of the descriptor. Patch versions are
  irrelevant. A different major fails in both directions: the artifact
  must run on the JVM version that built it. A mismatch is an error
  (exit 2) with a hint (`rig jvm install <n>`, or `JAVA_HOME`).
- **Offline by definition.** `rig launch` runs what a previous build
  already produced and cached. The lock is inert data: it ignores a
  stale lock, and `--frozen` has no effect on it.
- `rig launch` replaces its own process with the JVM: the exit code of
  the JVM is the exit code of the process.

## What is *not* configured

- There is no task engine: `rig exec` covers arbitrary commands.
- There is no alias inheritance: aliases are per-module. If you shared
  aliases before, share the `:extra-deps` requirement through
  `:rig/deps` instead.
- There is no separate deps-file or keypath indirection: Rig uses the
  `deps.edn` next to where you run the command, and resolves it by
  walking up.

!!! note "Legacy keys are read as fallback"
    During migration, the resolver still accepts the old
    `:exoscale.project/*` keys as fallbacks for the `:rig/*` keys above.
    The resolver recognizes `:slipset.deps-deploy/exec-args` only to
    detect a publish-enabled module; use `:rig/publish` for the actual
    deploy target. Prefer `:rig/*`; the migration guides remove the
    legacy keys entirely.

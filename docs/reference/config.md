# Configuration

All Rig configuration lives under one namespace — `:rig/*` — in the
`deps.edn` files. Standard tools.deps keys (`:paths`, `:deps`, `:aliases`)
are unchanged and documented by the tools.deps spec; Rig extends
`:mvn/repos` with an `:auth` marker (see
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
| `:rig/jvm` | — | The JVM the project runs on, e.g. `"21"` or `"21.0.10+7"` (Temurin). Recorded in the lock as an exact version; `rig` installs it into its state dir when missing (see [JVMs](#jvms-rigjvm)). Minimum supported value: `8` — the kernel jar is built for Java 8, so older JVMs cannot load it. |
| `:rig/compile-jvm-opts` | `[]` | JVM flags for the build/validate JVMs: the AOT build — the kernel JVM and the compile fork it launches — and the check namespace load/AOT-compile (see [JVM flags](#jvm-flags)). Version-sensitive flags like `--enable-preview` require a `:rig/jvm` pin. |
| `:rig/version-file` | `"VERSION"` | Root version file recorded in the lock for the root module. |

The root `deps.edn` may also carry its own `:deps`/`:aliases`/`:paths` —
the root is then resolved as a module (`"."`) in its own right.

## Module keys

Set in each module's `deps.edn`. All optional unless noted.

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
| `:rig/uber-opts` | `{}` | tools.build uberjar options (currently `:exclude`), e.g. `{:exclude ["META-INF/license/.*"]}`. |
| `:rig/timestamp-string` | — | ISO-8601 UTC instant (e.g. `"2026-01-01T00:00:00Z"`) used as the timestamp of every entry in the built jar and uberjar; entries are also written in name-sorted order. Set together with a pinned `:rig/jvm`, this makes the build output byte-reproducible. Pre-1980 values are refused (the zip format would clamp them silently). |
| `:rig/test?` | `true` | Recorded in the lock (`test.enabled`). `rig test` itself targets modules by their `:test` alias's `:exec-fn`. |
| `:rig/publish?` | `false` (true if `:rig/publish` is set) | Whether `rig publish`/`rig release` deploys this module. |
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

`rig test` runs whatever the module's `:test` alias declares — the standard
tools.deps `:exec-fn`:

```edn
:aliases
{:test {:extra-deps {lambdaisland/kaocha {:mvn/version "1.66.1034"}}
        :extra-paths ["test"]
        :exec-fn kaocha.runner/exec-fn
        :jvm-opts ["-Dlogback.configurationFile=test.logback.xml"]
        :env {"DB_URL" "localhost:5432"}}}
```

Any runner's `exec-fn` works (kaocha, clojure.test via a small fn, …).
`:jvm-opts` and `:env` from the alias are applied by `rig test`, `rig run
--alias`, and `rig repl --alias` exactly as the Clojure CLI would apply
them.

Other aliases (`:dev`, …) are first-class for `rig run --alias`, `rig repl
--alias`, `rig exec --alias`, and `rig tree --alias`; they are resolved
into the lock like `:test`.

## Prep functions (`:deps/prep-lib`)

The standard tools.deps prep-library key, honored by Rig: a function that
prepares a module before anything builds or runs it — code generation that
must run inside the module's own classpath, AOT compiling the module's
own Clojure for its dependents, compiling generated Java with an in-JVM
Maven, …:

```edn
;; a/proto/deps.edn
{:deps/prep-lib {:ensure "target/classes" :alias :prep :fn build/compile-java}
 :aliases       {:prep {:deps {io.github.clojure/tools.build
                               {:git/tag "v0.8.2" :git/sha "ba1a2bf"}}
                        :extra-paths ["build"]
                        :ns-default build}}}
```

- `:ensure` — a path or paths (relative to the module) the function
  guarantees. Rig re-runs the function when one is missing, and the
  build's clean step never deletes them — it wipes only the build's own
  previous output — so a class dir the prep owns survives `rig build`
  of the module itself and lands in the jar.
- `:alias` — an alias of the module; the function runs on the alias's
  **locked** classpath — Rig performs no re-resolution and passes the
  function no repository URLs (the function may still do its own
  in-JVM work, e.g. a `b/create-basis` against `~/.m2`). The alias
  typically declares the build file under `:extra-paths` and the
  function's namespace under `:ns-default`.
- `:fn` — a var resolved on the prep classpath, called with a single
  `nil` argument — the way tools.deps' `exec-prep!` invokes it when the
  alias declares no `:exec-args` — so declare it `[f]` or `[& _]`, not
  `[]`. The child's exit status propagates: a failing prep fails the
  command.

`rig build`, `rig test`, `rig run` and `rig repl` prepare the target
module and every local module on its classpath that declares a prep (or
`:rig/java-src-dirs`), in dependency order. The function re-runs when a
`:ensure` path is missing, when its stamp is out of date (the manifest
hash, a content digest of the module's sources, a digest of the locked
dependencies, or the function name changed), or when a local dependency
was re-prepped in the same invocation. A fresh run records its digests
under `target/.rig-prep.json`; `rig clean` removes the output and the
stamp together. The function's JVM gets the module's `:jvm-opts` and the
prep alias's `:jvm-opts`, and runs from the module's directory.

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
publish` uploads, and the resolver's artifact traffic (see the auth proxy
below) — carries `Authorization: Bearer <token>`.

### OIDC gates

An OIDC gate is an identity provider and the audience/client its tokens
must carry, plus the repository URL prefixes it fronts. Gates are shared
across an organization, so they live in one config file, not in any
`deps.edn`:

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

A marked repo is bound to the gate that fronts its `:url` (longest-prefix
match, on the URL): exactly one matching gate is used; several matching
gates fail fast ("tighten the gate urls"); none falls back to the single
url-less gate, and when there is no match at all the command fails fast,
before any resolution work, naming the repo, the config file, and the
missing prefix.

### Resolving a gate's token

Each gate's bearer is resolved once per command, per repo:

1. `RIG_TOKEN_<GATE>` when set — the gate name uppercased, dashes turned
   into underscores (`pier` → `RIG_TOKEN_PIER`). This is the CI path: the
   runner injects the token.
2. Else the gate's cached token in the state dir
   (`~/.local/share/rig/oidc/`, one file per gate, valid until 30s before
   its expiry).
3. Else a fresh negotiation with the gate's issuer: the browser flow
   (authorization code + PKCE, opened in your browser) when a browser is
   available, else the device-code flow (RFC 8628) in headless
   environments. The issued token is verified against the issuer's JWKS
   (signature, issuer, audience, expiry) before it is used or cached.

`rig auth get [gate|url]` prints one gate's token to stdout, using the
same chain:

```console
$ rig auth get pier          # by gate name
$ rig auth get https://maven.corp.example   # or by a repo URL it fronts
eyJhbGciOi...
```

With no argument, the config must hold exactly one gate. `--flow
browser|device` forces the negotiation flow (auto by default).

### Auth proxy

tools.deps (MIMA) cannot carry a bearer of its own, so Rig does not hand
it the real repo URL: for every resolution run Rig starts a local
loopback proxy and rewrites the `:url` of each marked repo to it. The
proxy forwards every Maven request — exact versions and floating ones
alike — to the repo's real URL with **that repo's** bearer attached,
streaming the response without touching disk. The tokens never leave
Rig's process memory, and the proxy dies with the command. The repo id is
unchanged, so lock attribution (and Maven's `_remote.repositories`) still
point at the original repo, and the lock records its original URL.
Floating versions are probed by the kernel against the original URL, with
the per-repo bearer and subject to the cooldown; the artifacts themselves
always come through the proxy.

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

- `rig lock` records the pin in `deps.lock` as an **exact** version
  (e.g. `21.0.12.1+1`), resolved from the Adoptium API at lock time.
  Re-locks keep the locked version; `rig jvm update` advances it to the
  newest release still satisfying the manifest pin.
- When a command needs a JVM and the locked one is not installed, `rig`
  downloads, sha256-verifies and installs it (one visible line). Under
  `--offline` this fails with a hint instead.
- Managed JDKs live in the state dir
  (`~/.local/share/rig/jdks/temurin-<version>/`); `rig jvm list` shows them
  alongside the system `java`. Launched processes get
  `JAVA_HOME` set to the managed JDK.
- Projects scaffolded with `rig new` start with the current LTS pin (omitted
  under `--offline`, or when the Adoptium lookup fails).
- Without the pin, `rig` uses `JAVA_HOME`, then `java` on `PATH`. When
  neither is available, the error suggests installing the current LTS (the
  LTS feature is looked up from the Adoptium info endpoint; nothing is
  installed automatically). `RIG_JAVA=<path>` overrides everything (dev
  override, like `RIG_KERNEL_JAR`).
- The pin is also the **bytecode floor** of the module's build output:
  every class in the jar or uberjar `rig build` produces (dependency
  classes included, for the uberjar) must load on the pinned JVM, and the
  build fails listing the offending entries when one does not. A
  multi-release jar entry is only checked at the JVM versions where it
  can load. Without the pin there is no floor and no check.

Vendor: Temurin (Eclipse Adoptium), GA releases only.

## JVM flags

Three contexts, three keys — every JVM Rig launches gets its flags from the
key that matches how the JVM is used:

| Key | Declared in | Applies to |
|---|---|---|
| `:rig/compile-jvm-opts` | workspace (root manifest) | The AOT build — kernel JVM and the compile fork it launches (`rig build`) — and the check namespace load/AOT-compile (`rig check`, stage 2). |
| `:jvm-opts` | module (standard tools.deps key) | Dev execution: `rig run`, `rig repl`, `rig exec`, `rig test`, plus the module's prep function. |
| `:rig/launch-opts` | module | `rig launch` only. Baked into the artifact's launch descriptor at build time, so it travels with the jar. |

The `rig build` JVMs (kernel and compile fork), `rig check` and `rig
launch` never read `:jvm-opts` — the exception is the module's prep
function, which runs as part of a build and takes `:jvm-opts` like any
dev execution; dev execution never reads `:rig/launch-opts`. (`rig check`'s stage-1 metadata
kernel op runs bare — it inspects the lock, not the code, so it needs no
workspace flags.)

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

A lock with `--enable-preview` in `:rig/compile-jvm-opts` but no `:rig/jvm`
pin is rejected at load time: the preview set depends on the JVM version,
and Rig will not guess it.

## Native images (`rig build --native`)

`:rig/native?` declares that a module builds a standalone native-image
binary (GraalVM):

```edn
{:rig/native? true :rig/main app.core}
```

- **GraalVM.** The version is derived from the workspace's `:rig/jvm`
  pin — `--native` without a pin is a usage error — and recorded in the
  lock as an exact release (`graalvm: {vendor, requested, version}`,
  e.g. `21.0.2`). The build never downloads: when the locked GraalVM is
  not installed, `rig build --native` fails with a hint — install it
  first with `rig graalvm install <version>` (downloaded from the
  `graalvm/graalvm-ce-builds` GitHub releases, sha256-verified, into the
  state dir `~/.local/share/rig/graal/`). Manage them with `rig graalvm`
  (install / list / uninstall / update). `RIG_GRAALVM_HOME=<home>`
  overrides the store (a dev override, like `RIG_JAVA`; it must contain
  `bin/native-image`).
- **Entry point.** `:rig/main` must be a Clojure namespace. Rig compiles
  a small entry shim (javac beside the workspace's java) whose main
  delegates to `clojure.main` with `-m <ns>`, so the binary runs with
  plain args: `<binary> arg1 arg2`.
- **Class initialization.** A native image cannot load classes from a
  classpath at run time: Rig loads the Clojure runtime and the module's
  namespace at image build time, and marks every namespace package it
  finds on the classpath for build-time initialization. A namespace
  reached only dynamically (outside the transitive `require` closure of
  `:rig/main`) must be `require`ed at top level, else the build fails
  naming the missing class.
- **Arguments.** Rig passes the fixed arguments `--no-fallback`,
  `--class-path <class dir>:<locked classpath>`,
  `--initialize-at-build-time=<namespace packages>`, and
  `-o <native-file>`; `:rig/native-opts` is appended after them. A
  repeated `--initialize-at-build-time` in the opts adds to Rig's list;
  `--fallback` conflicts with Rig's `--no-fallback`.
- **Prerequisites.** A full JDK (the shim is compiled with `javac`), a C
  compiler for linking, and a few GB of RAM. A native build takes
  minutes, not seconds.
- **Reflection.** Rig does not manage native-image configuration in v1:
  `META-INF/native-image/**` entries in dependency jars are
  auto-discovered, and `:rig/native-opts` is the escape hatch
  (`--features=…`, `--initialize-at-build-time=…`, …).

## Production launch (`rig launch`)

`rig launch` runs the built artifact with Rig's production JVM flag set.
The flag order is fixed:

1. **Rig's defaults** — G1 garbage collection (`-XX:+UseG1GC`, with
   `-XX:+AlwaysPreTouch`), exit on out-of-memory
   (`-XX:+ExitOnOutOfMemoryError`, plus
   `-XX:+HeapDumpOnOutOfMemoryError`), and loopback-only JMX on port
   **10101** (`-Dcom.sun.management.jmxremote`, `authenticate=false`,
   `ssl=false`; the RMI port is pinned to 10101 and
   `-Djava.rmi.server.hostname=127.0.0.1` keeps JMX to same-host
   clients). There is no `MaxRAMPercentage` and no GC logging in the
   defaults.
2. **the module's `:rig/launch-opts`** (from the module manifest, or from
   the baked launch descriptor when launching a jar standalone) — later
   flags override Rig's defaults (last JVM flag wins; a repeated `-D`
   re-sets the property). A garbage collector in `:rig/launch-opts`
   (e.g. `-XX:+UseZGC`) *replaces* the G1 default: the JVM refuses to
   start with two collectors selected, so Rig drops its own rather than
   pass both.

The JMX port is fixed at 10101; if it collides in your environment,
override both `-Dcom.sun.management.jmxremote.port=…` and
`-Dcom.sun.management.jmxremote.rmi.port=…` from `:rig/launch-opts`.

### The launch descriptor

`rig build` bakes the artifact's launch plan into every jar and uberjar as
`META-INF/rig/launch.json`:

```json
{"version": 1, "rig": "0.4.0", "main": "app.core",
 "jvm-opts": ["-Xmx2g"], "java": 21, "uber": true}
```

`main`, `jvm-opts` (the module's `:rig/launch-opts` — the JSON field name
predates the key and stays, so old artifacts keep launching), `java` (the
build JVM's feature version) and `uber` —
all taken from the lock at build time. Rig's production defaults are *not*
baked in: the launching Rig applies them, so the policy follows Rig
upgrades even for old artifacts. `rig launch <jar>` reads the descriptor
first, so a built jar launches standalone, without the workspace.

### Behavior

- **Jar selection.** The first positional is the jar, when it names an
  existing file; otherwise (in a workspace) the jar is the target module's
  build output from the lock (the uberjar when one is declared) and all
  positionals go to the main.
- **Non-uber jars** launch with the locked classpath, the module's own
  source paths replaced by the built jar — which is why they only launch
  inside the workspace.
- **Java version.** The launch JVM's major version must *exactly* match the
  descriptor's `java` (patch versions are irrelevant; a different major is
  refused in both directions — the artifact runs on the JVM it was built
  with). Mismatch is an error (exit 2) with a hint
  (`rig jvm install <n>`, or `JAVA_HOME`).
- **Offline by definition.** `rig launch` never touches the network: no
  JDK auto-install, no artifact fetch, no update notice. The lock is inert
  data — launch never checks staleness, never re-locks, and `--frozen` has
  no effect on it.
- The child's exit code is Rig's.

## What is *not* configured

- There is no task engine: `rig exec` covers arbitrary commands.
- There is no alias inheritance: aliases are per-module. If you shared
  aliases before, share the `:extra-deps` requirement through
  `:rig/deps` instead.
- There is no separate deps-file or keypath indirection: `deps.edn` next to
  where you run the command, resolved by walking up.

!!! note "Legacy keys are read as fallback"
    During migration, the resolver still accepts the old
    `:exoscale.project/*` keys as fallbacks for the `:rig/*` keys above.
    `:slipset.deps-deploy/exec-args` is recognized only to detect that a
    module was publish-enabled; use `:rig/publish` for the actual deploy
    target. Prefer `:rig/*`; the migration guides remove the legacy keys
    entirely.

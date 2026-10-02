# Production launch

`rig launch` is the entrypoint for a deployed app: it runs the built
artifact with Rig's production JVM flag set — G1 (`AlwaysPreTouch`),
exit on out-of-memory (with heap dump), loopback-only JMX on port
**10101** — and nothing else. It is offline by definition: it runs the
artifact it is pointed at, with what is already built and cached — and
the app's exit code is the process's. The launch plan (main, launch opts, build JVM) is baked
into the artifact by `rig build`, so a built jar is self-describing. The
full flag reference:
[Production launch](../reference/config.md#production-launch-rig-launch).

## Declaring launch opts

The module being launched declares its production flags with
`:rig/launch-opts` in its `deps.edn`:

```edn
{:rig/lib        app.core
 :rig/main       app.core
 :rig/uberjar?   true
 :rig/launch-opts ["-Xmx2g"]}
```

Rig applies its defaults first, then the module's opts — the last JVM
flag wins. Two things to know:

- a garbage collector in `:rig/launch-opts` (e.g. `-XX:+UseZGC`)
  *replaces* the G1 default: the JVM refuses to start with two
  collectors selected, so Rig drops its own rather than pass both;
- the JMX port is fixed at 10101; to move it, override *both*
  `-Dcom.sun.management.jmxremote.port=…` and
  `-Dcom.sun.management.jmxremote.rmi.port=…`.

At build time the opts are baked into the jar's launch descriptor
(`META-INF/rig/launch.json`), so they travel with the artifact: an
image launches with the flags of the jar it ships, while Rig's default
set is applied by the launching binary — the policy follows Rig
upgrades even for old artifacts.

`:rig/launch-opts` never reaches dev execution (`rig run`, `rig repl`,
`rig test`, … — that is `:jvm-opts`), and `:rig/compile-jvm-opts` never
reaches it either: see [JVM flags](../reference/config.md#jvm-flags) for
the three contexts.

### Per-deployment overrides: `RIG_LAUNCH_OPTS`

`:rig/launch-opts` is the stable, declared set — it travels with the
artifact. For per-deployment tuning, set `RIG_LAUNCH_OPTS` in the
environment (whitespace-separated JVM flags). It appends after
`:rig/launch-opts`, with the same rules: the last JVM flag wins (a
repeated `-D` re-sets the property), and a collector there replaces any
earlier selection:

```sh
docker run -e RIG_LAUNCH_OPTS=-Xmx4g img
```

```yaml
# k8s: same image, a different heap per workload
env:
  RIG_LAUNCH_OPTS: "-Xmx4g"
```

### Choosing between `:rig/launch-opts` and `RIG_LAUNCH_OPTS`

The decision is scope, not capability: `RIG_LAUNCH_OPTS` can do
anything the declaration can, but it applies to **one** process launch,
while `:rig/launch-opts` is baked into the artifact and applies to
*every* launch of that jar — every deployment, every image built from
it. So ask: is the flag a property of the app, or of the place it runs?

**`:rig/launch-opts`** — what the app always needs, the same in every
deployment:

- the garbage collector that fits the app (ZGC for a latency-bound
  large-heap service, Serial for a tiny sidecar — the app's allocation
  and pause profile, not the machine's);
- GC tuning the app depends on (pause targets, …);
- `-D` properties that configure the app or its libraries identically
  everywhere;
- a standing deviation from a Rig default that holds in all
  deployments (e.g. JMX reachable from outside, in every deployment).

**`RIG_LAUNCH_OPTS`** — what varies with the environment or the traffic
actually received:

- heap sizing (`-Xmx`, `-Xms`, `-XX:MaxRAMPercentage`) — driven by the
  container's memory limit and the workload, different per deployment;
- a collector experiment or canary — one cluster on a different GC,
  without touching the artifact;
- per-deployment JMX (a different port, external access in one
  environment only);
- per-deployment `-D` properties (log level, environment markers);
- incident diagnostics — an `-Xlog:gc*` for a latency investigation
  goes here, not in the manifest.

The two anti-patterns: stable, always-on flags in `RIG_LAUNCH_OPTS`
don't travel with the artifact — every deployment must set them, and a
rebuilt environment loses them; environment-dependent sizing in
`:rig/launch-opts` is baked in — changing it means a rebuild and a new
image, and one size rarely fits all deployments.

The full flag semantics — what each default does, and what happens
mechanically when a later stage overrides — is in
[Production launch](../reference/config.md#production-launch-rig-launch).

## Running

```sh
rig launch                          # in the workspace: the lock's jar, args to the main
rig launch -p modules/app           # pick the module
rig launch target/app-uber.jar --env prod
```

The first positional is the jar, when it names an existing file;
otherwise (in a workspace) all positionals go to the main and the jar is
the module's build output from the lock (the uberjar when one is
declared). The launch JVM's major version must exactly match the build
JVM's — the artifact runs on the JVM it was built with; a mismatch is an
error (exit 2), as is a missing artifact. A plain, non-uber jar launches
with the locked classpath, so it only launches inside the workspace; an
uberjar launches standalone.

## Process & lifecycle

`rig launch` does not run the JVM as a child: it replaces its own
process with the JVM (process `exec`). The launched app *is* the
process — in a container, its PID 1:

- the container's signals reach the JVM directly — there is no wrapper
  process to forward them;
- the JVM's exit code is the process's exit code;
- on the terminal, Ctrl+C (`SIGINT`) reaches the JVM and runs its
  shutdown hooks.

In a container, the stop path is hook-driven, not a hard kill. The
JVM *does* run shutdown hooks on `SIGTERM`: HotSpot installs handlers
for `SIGTERM` (and `SIGINT`, `SIGHUP`) and triggers the
`Runtime.addShutdownHook` hooks when they arrive (verified on Temurin
21). `docker stop` and k8s termination deliver `SIGTERM` to the JVM as
PID 1, so the app's hooks run. The caveats:

- the hooks must finish before the termination grace period expires,
  or the runtime escalates to `SIGKILL`; `SIGKILL` — expired grace or
  `docker kill` — kills the JVM with nothing running. Size k8s
  `terminationGracePeriodSeconds` (or `docker stop -t`) to the app's
  cleanup time.
- `-Xrs`, if the app opts in via `RIG_LAUNCH_OPTS`, disables this
  mechanism; a stop then becomes a hard kill.

(Windows has no process `exec`: rig forks the JVM and waits,
propagating its exit code.)

## Config & logging

`RIG_LAUNCH_OPTS` is for JVM flags only — app configuration is plain
environment variables and arguments: `docker run img --env prod` passes
`--env prod` to the main. The JVM inherits the container's stdout and
stderr, so the app's prints are the container log: log to stdout, not to
files.

## The Docker image

The production image is three parts: the JDK the project declares,
Rig's binary, and the artifact.

```dockerfile
# the declared JDK — :rig/jvm is "21" in the root deps.edn
FROM eclipse-temurin:21-jdk-jammy AS build
COPY --from=ghcr.io/brutasse/rig:latest /usr/local/bin/rig /usr/local/bin/
COPY --from=ghcr.io/brutasse/rig:latest /root/.local/share/rig /root/.local/share/rig
WORKDIR /app
COPY . .
RUN rig verify --frozen && rig test --frozen
RUN rig build --uber --frozen -p modules/app

FROM eclipse-temurin:21-jre-jammy
COPY --from=ghcr.io/brutasse/rig:latest /usr/local/bin/rig /usr/local/bin/
COPY --from=build /app/modules/app/target/app-uber.jar /app/app.jar
WORKDIR /app
EXPOSE 10101
ENTRYPOINT ["rig", "launch", "/app/app.jar"]
```

- **The base is the declared JDK.** `:rig/jvm` is the contract: the
  build stage is the JDK the project declares. No rig-managed JDK for
  the pin is installed in the copied state dir, so the build compiles
  against the image's JDK — the matching system `java` satisfies the
  pin. The runtime image carries the same JDK as a JRE: the launch
  descriptor records the build JVM's major version and `rig launch`
  refuses a different one. A JRE is enough: nothing is compiled at
  runtime.
- **Copy rig into the JDK layer.** The build stage takes the binary
  and Rig's state dir (the pre-baked resolver kernel and rig.runner
  jars) out of the Rig image: `rig verify` and `rig test` need them, and
  pre-baked means no fetch from GitHub at build time.
- **The lock is the contract.** The build consumes the committed
  `deps.lock`: `rig verify --frozen` fails when it is missing or stale
  (exit 3) — locking is workstation work, and a dependency change is a
  lock change to commit.
- **Copy only the binary into the JRE layer.** `rig launch` never
  touches the resolver kernel or the runner, and a standalone uberjar
  is self-describing — the launch plan is baked into the jar, no lock,
  no state dir. The JRE's own `java` is the launch JVM.
- **Pin both bases.** Rig's `latest` floats: in production, copy from
  the release tag you tested (`ghcr.io/brutasse/rig:vX.Y.Z`), and pin
  the `eclipse-temurin` tags to the declared JDK.
- **Rig is the entrypoint.** `rig launch` replaces its own process with
  the JVM, so the container's process *is* the app. `CMD` arguments
  (`docker run img --env prod`) pass to the app's main; the app's exit
  code is the container's.
- **JMX.** Port 10101 binds to loopback — reachable from inside the
  container only; `EXPOSE` it for same-host tooling, and expose the
  app's own port separately. Health probes must hit the app's port,
  never JMX. To reach JMX from outside, override the JMX flags from
  `:rig/launch-opts`.

## When the workspace must come along

A plain (non-uber) jar keeps its classpath in the lock, so it launches
inside the workspace — the image carries the workspace and the
artifact cache:

```dockerfile
FROM eclipse-temurin:21-jdk-jammy AS build
COPY --from=ghcr.io/brutasse/rig:latest /usr/local/bin/rig /usr/local/bin/
COPY --from=ghcr.io/brutasse/rig:latest /root/.local/share/rig /root/.local/share/rig
WORKDIR /app
COPY . .
RUN rig verify --frozen && rig build --frozen -p modules/app

FROM eclipse-temurin:21-jre-jammy
COPY --from=ghcr.io/brutasse/rig:latest /usr/local/bin/rig /usr/local/bin/
COPY --from=build /root/.local/share/rig /root/.local/share/rig
COPY --from=build /app /app
WORKDIR /app
EXPOSE 10101
ENTRYPOINT ["rig", "launch"]
```

The jar comes from the lock; `CMD` arguments still go to the main. See
[Docker](docker.md) for the multi-stage, non-root and offline variants
of this pattern.

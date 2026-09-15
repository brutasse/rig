# All artifacts through one repository

Route every Maven artifact through a single [Pier](https://brutasse.github.io/pier/)
instance: one OIDC-gated URL that caches Maven Central and Clojars on demand,
and serves your corporate jars from the same bucket.

## What you get

- **One entry point.** Every version probe, every download, every lock
  attribution goes through the same gated URL. Nothing talks to Central,
  Clojars, or the corporate repo directly.
- **One trust boundary.** One CEL policy, one set of OIDC issuers, one S3
  bucket. rig still hash-pins every artifact byte
  ([security model](../concepts/security.md)).
- **The public world cached once.** Pier's pull-through caches Central and
  Clojars artifacts on first request; a cold read hits the upstream, a warm
  read never does.

This covers the Maven artifacts in `deps.lock`. The resolver kernel jar (a
pinned GitHub release) and managed JDKs (Adoptium) are not Maven artifacts
and keep their own verified sources.

## The Pier side

Configure the instance with the two public upstreams and your corporate
groups reserved:

```yaml
upstream:
  - name: central
    url: https://repo.maven.apache.org/maven2
  - name: clojars
    url: https://repo.clojars.org
reserved_groups:
  - com.acme
```

Everything under `com.acme` is internal-upload-only — never pulled from an
upstream, metadata synthesized by Pier — and every other path is
pull-through, read-only. Configure the OIDC issuers and CEL policy rules as
usual; see the [Pier documentation](https://brutasse.github.io/pier/):
[quickstart](https://brutasse.github.io/pier/quickstart/),
[pull-through cache](https://brutasse.github.io/pier/pull-through/),
[policy rules](https://brutasse.github.io/pier/policies/).

## The rig side

rig ships with two built-in repositories, `central` and `clojars`.
Redeclaring an id under `:mvn/repos` **replaces** the built-in one — the
same rule tools.deps applies to its own resolution. The resolver probes
`:mvn/repos` in map order, first match wins, and Pier answers every probe
— public via pull-through, corporate from its reserved groups — so
`central` is the only entry resolution strictly needs:

```edn
:mvn/repos {"central" {:url "https://pier.example" :auth :oidc}
            "clojars" {:url "https://pier.example" :auth :oidc}}
```

Everything rig does for Maven artifacts then goes through the redeclared
repository:

- floating-version probes (repository metadata),
- the native m2 resolution of `rig lock`,
- the lock's URL attribution — and therefore `rig verify`,
- `rig add`'s newest-version lookup.

- Redeclaring `central` routes every artifact — public and corporate —
  through Pier, and the lock attributes them all to `central`, the first
  repo probed.
- Redeclaring `clojars` is what makes it airtight: it replaces the built-in
  real Clojars, so nothing can ever reach `repo.clojars.org`. Without it,
  resolution still works (Pier-as-`central` also serves Clojars artifacts),
  but the real Clojars lingers as a fallback.

`:rig/publish` needs an id to publish to; it works with that same
`central` id, or add a dedicated `corp` id (the same URL) if you want a
cleaner publish label. It does not affect attribution.

rig does not inherit `:mvn/repos`: declare the map in the root manifest
**and** in every module manifest.

## Authentication

Mark the repositories `:auth :oidc`. The gate that fronts the Pier URL
lives in the shared gate config (`~/.config/rig/auth.yaml`):

```yaml
gates:
  pier:
    url: https://pier.example
    well-known: https://idp.example/.well-known/openid-configuration
```

The bearer is resolved per gate, once per command: `RIG_TOKEN_PIER` when
set (the CI path — the runner injects it), else the gate's cached token,
else a fresh negotiation with the issuer (browser flow, or device code in
headless environments), verified against the issuer's JWKS. A command
fails fast, before any resolution work, when the repo's URL is fronted by
no gate. [Authenticated
repositories](../reference/config.md#authenticated-repositories-auth-oidc)
covers the gate config, the token chain and the auth proxy in detail.

## Publishing corporate artifacts

`:rig/publish` resolves its `:repo` through the `:mvn/repos` URLs, so
publishing goes through Pier with the same bearer — to the `central` id
(redeclared to the Pier URL), or to a dedicated `corp` id if you added one:

```edn
:rig/publish {:repo "central"}
```

`rig release` (and `rig publish`) PUT the module's artifacts to the Pier
URL. Pier accepts uploads only under the reserved groups — `com.acme`
here — and keeps every other path read-only.

Pin corporate artifacts at explicit versions
(`rig add com.acme/thing 1.2.3`). Pier synthesizes the artifact-level
metadata when reserved-group artifacts are published, with a `lastUpdated`
of that publish time — so a floating version of a private artifact hits the
[cooldown](../concepts/security.md#cooldowns-the-adoption-window), and each
re-publish resets it. Public artifacts are unaffected: their upstream
metadata applies as usual (proxied metadata is revalidated against the
upstream on a 24h TTL by default).

## CI

In CI the runner injects a short-lived token as `RIG_TOKEN_PIER` (the
gate's environment variable); nothing static lives in the repository or in
rig's state. The usual CI gate
([ci.md](ci.md)) works unchanged — `rig verify --frozen` fetches from the
lock's URLs, i.e. from Pier, with the bearer.

# All artifacts through one repository

Route every Maven artifact through a single [Pier](https://brutasse.github.io/pier/)
instance: one OIDC-gated URL that caches Maven Central and Clojars on demand,
and serves your corporate jars from the same bucket.

## What you get

- **One entry point.** Every version probe, every download, every lockfile
  attribution goes through the same gated URL. Nothing talks to Central,
  Clojars, or the corporate repo directly.
- **One trust boundary.** One CEL policy, one set of OIDC issuers, one S3
  bucket. Rig still hash-pins every artifact byte
  ([security model](../concepts/security.md)).
- **The public world cached once.** Pier's pull-through cache stores Central
  and Clojars artifacts on the first request. A cold read hits the upstream.
  A warm read never does.

This covers the Maven artifacts in `deps.lock`. The resolver kernel, the
runner jars (pinned GitHub releases), and the managed JDKs (Adoptium) are
not Maven artifacts. They keep their own verified sources.

## The Pier side

Configure the instance with the two public upstreams and your reserved
corporate groups:

```yaml
upstream:
  - name: central
    url: https://repo.maven.apache.org/maven2
  - name: clojars
    url: https://repo.clojars.org
reserved_groups:
  - com.acme
```

Everything under `com.acme` is internal-upload-only. Pier never pulls it
from an upstream, and Pier synthesizes its metadata. Every other path is
pull-through and read-only. Configure the OIDC issuers and CEL policy rules
as usual. See the [Pier documentation](https://brutasse.github.io/pier/):
[quickstart](https://brutasse.github.io/pier/quickstart/),
[pull-through cache](https://brutasse.github.io/pier/pull-through/),
[policy rules](https://brutasse.github.io/pier/policies/).

## The Rig side

Rig ships with two built-in repositories: `central` and `clojars`.
Redeclaring an id under `:mvn/repos` **replaces** the built-in one.
tools.deps applies the same rule to its own resolution. The resolver probes
`:mvn/repos` in map order, and the first match wins. Pier answers every
probe: public artifacts via pull-through, corporate artifacts from its
reserved groups. So `central` is the only entry that resolution strictly
needs:

```edn
:mvn/repos {"central" {:url "https://pier.example" :auth :oidc}
            "clojars" {:url "https://pier.example" :auth :oidc}}
```

Everything Rig does for Maven artifacts then goes through the redeclared
repository:

- floating-version probes (repository metadata),
- the native m2 resolution of `rig lock`,
- the lockfile's URL attribution — and therefore `rig check`,
- the newest-version lookup of `rig add`.

- Redeclaring `central` routes every artifact — public and corporate —
  through Pier, and the lockfile attributes them all to `central`, the
  first repo probed.
- Redeclaring `clojars` makes it airtight: it replaces the built-in real
  Clojars, so nothing can ever reach `repo.clojars.org`. Without it,
  resolution still works, because Pier-as-`central` also serves Clojars
  artifacts. But the real Clojars stays as a fallback.

`:rig/publish` needs an id to publish to. It works with that same `central`
id. Add a dedicated `corp` id (the same URL) if you want a cleaner publish
label. That does not affect attribution.

Rig does not inherit `:mvn/repos`: declare the map in the root manifest
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

Rig resolves the bearer per gate. It uses `RIG_TOKEN_PIER` when set —
Rig uses an environment token as-is and never re-negotiates it. If that
is empty, it uses the cached token of the gate. Otherwise it negotiates
a fresh token. When the gate fronts the GitHub Actions issuer, the
negotiation uses the runner's id-token endpoint (see [CI](#ci)).
Otherwise it uses the issuer's browser or device-code flow. Rig
verifies the token against the issuer's JWKS.
When no gate fronts the repo's URL, a command fails fast, before any
resolution work. [Authenticated
repositories](../reference/config.md#authenticated-repositories-auth-oidc)
covers the gate config, the token chain, and the auth proxy in detail.

## Publishing corporate artifacts

`:rig/publish` resolves its `:repo` through the `:mvn/repos` URLs, so
publishing goes through Pier with the same bearer. Publish to the `central`
id (redeclared to the Pier URL), or to a dedicated `corp` id if you added
one:

```edn
:rig/publish {:repo "central"}
```

`rig publish` PUTs the module's artifacts to the Pier URL. Pier accepts
uploads only under the reserved groups (`com.acme` here), and keeps every
other path read-only.

Pin corporate artifacts at explicit versions
(`rig add com.acme/thing 1.2.3`). When you publish a reserved-group
artifact, Pier synthesizes the artifact-level metadata with a `lastUpdated`
of that publish time. So a floating version of a private artifact hits the
[cooldown](../concepts/security.md#cooldowns-the-adoption-window), and each
re-publish resets it. Public artifacts keep their normal behavior: their
upstream metadata applies. By default, Pier revalidates proxied metadata
against the upstream on a 24h TTL.

## CI

On GitHub Actions you do not mint a token or pass one around. Point the
gate at the GitHub Actions issuer, set `audience` to the value your
registry validates, and give the workflow the `id-token: write`
permission:

```yaml
# ~/.config/rig/auth.yaml — the shared gate config
gates:
  pier:
    url: https://pier.example
    well-known: https://token.actions.githubusercontent.com/.well-known/openid-configuration
    audience: https://pier.example
```

```yaml
# .github/workflows/ci.yml
permissions:
  id-token: write
```

Rig exchanges the runner's credentials
(`ACTIONS_ID_TOKEN_REQUEST_URL`, `ACTIONS_ID_TOKEN_REQUEST_TOKEN`) for
an id token on demand. Nothing static lives in the repository or in
Rig's state.

A GitHub Actions id token lives 5 minutes; the runner's credentials
live for the whole job. So a long job keeps working: when the registry
rejects the expired bearer, Rig exchanges a fresh one. The usual CI
gate ([ci.md](ci.md)) works unchanged: `rig check` fetches from the
lockfile's URLs — that is, from Pier — with the bearer.

For a runner that is not GitHub Actions, or a token minted by another
workflow, set `RIG_TOKEN_PIER` instead. Rig uses that token as-is and
never re-negotiates it.

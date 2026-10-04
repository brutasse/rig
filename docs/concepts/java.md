# Java sources

Rig compiles Java in two situations: when you build a module that declares
`:rig/java-src-dirs`, and automatically, when a module you build depends on
another local module that declares them.

## Your own module

A module with Java sources declares the directories:

```edn
{:rig/java-src-dirs ["java"]}
```

`rig build` first compiles them with javac into the module's class dir,
then compiles its Clojure. This way, Clojure code that references the
module's own Java classes compiles. The classes land in the jar, the
uberjar, and the native image, like anything else compiled into the class
dir. Set options in `:rig/javac-opts`. When the workspace pins `:rig/jvm`,
Rig prepends `--release <n>`. Rig skips this when your opts already set the
source level.

The class dir also rides on the module's own classpath, after its `:paths`:
`rig test`, `rig run` and `rig check` see the module's compiled Java
classes, the way Lein puts `target/classes` on the classpath. A module that
declares `:rig/java-src-dirs` does not need to list its class dir in
`:paths` — and listing it never duplicates the entry.

## Local dependency modules

This feature exists for this situation: module `b` depends on module `a` by
`:local/root`, and only compiled Java classes make `a` usable (generated
protobuf classes, JNI bindings, …). `b` cannot compile until `a`'s classes
exist.

Two declarations make it work, both in `a`:

```edn
;; a/deps.edn
{:paths            ["src" "resources" "target/classes"]  ;; (1)
 :deps             {…}
 :rig/java-src-dirs ["src"]}                             ;; (2)
```

1. The class dir must reach the classpath. A module that declares
   `:rig/java-src-dirs` gets its class dir appended automatically, so the
   `:paths` entry below is not needed. When the classes come from a prep
   function instead, `:paths` must include the class dir: Rig flows a local
   dependency's `:paths` into every dependent's classpath, and that is how
   `b` sees `a/target/classes`.
2. `:rig/java-src-dirs` points at `a`'s Java sources.

With that, `rig build`, `rig test`, and `rig run` of `b` (or of anything
downstream of `a`) compile `a` with javac automatically before `b`
compiles. Rig runs javac on `a`'s **locked** base classpath, into `a`'s
class dir. This javac is the same one a full build of `a` runs. It carries
the same staleness tracking as everything else in Rig. A stamp under
`a/target/` records the manifest hash, a content digest of the sources, and
a digest of the locked dependencies. If any of these three changes, or if
Rig re-preps a dependency of `a`, the javac runs again. `rig clean` removes
the output and the stamp.

Rig does not re-resolve: the classpath javac uses is the locked one Rig
already fetched and hashed. There is no in-JVM Maven step, so the prep
never needs a dependency's repository credentials.

## Migrating away from `:deps/prep-lib`

Before, the common shape for "compile the generated Java before anyone else
builds" was a tools.deps prep library:

```edn
;; before — a/proto/deps.edn
{:paths           ["src" "resources" "target/classes"]
 :deps            {…}
 :deps/prep-lib   {:ensure "target/classes" :alias :prep :fn compile-java}
 :aliases         {:prep {:deps {io.github.clojure/tools.build
                                  {:git/tag "v0.8.2" :git/sha "ba1a2bf"}}
                          :extra-paths ["build"]
                          :ns-default build}}}

;; a/proto/build/build.clj
(defn compile-java [& _]
  (b/javac {:src-dirs ["src"] :class-dir "target/classes"
            :basis (b/create-basis {:project "deps.edn"})}))
```

If the prep function only compiles the module's own Java sources into its
own class dir, Rig already does that natively. How to migrate:

1. add `:rig/java-src-dirs` to the proto module (point it at the Java
   sources);
2. drop the class dir from `:paths` (Rig appends a Java module's class dir
   for consumers; keeping the entry is harmless);
3. delete `:deps/prep-lib`, the `:prep` alias, the `build.clj`, and the
   tools.build git dependency from the prep classpath;
4. `rig lock` again (the lock records `java-src-dirs` and drops the
   `:prep` alias).

Consumers change nothing. They still depend on the proto module by
`:local/root`, and Rig prepares it for them. Afterwards, the manifest is
self-contained: no build file that Rig does not read, no second classpath,
no separate function whose calling convention you have to know.

If a module declares **both** `:rig/java-src-dirs` and a prep function, Rig
runs the javac first and then the prep function. The function's classpath
leads with the freshly compiled classes. The javac does not clear the
prep's `:ensure` output. A clean wipes only the build's own previous
output. Each keeps its own staleness stamp.

## When to keep `:deps/prep-lib`

Keep the prep library when the preparation is not plain javac. Examples:
AOT compiling the module's own Clojure for its dependents, code generation
that must run inside the module's own classpath, and anything else custom.
Rig runs the function on the locked `:prep` alias classpath, verifies the
declared `:ensure` output, and tracks staleness exactly as for the native
javac. Rig calls the function with a single `nil` argument. That is the way
tools.deps' `exec-prep!` invokes it when the alias declares no
`:exec-args`. So declare it `[f]` or `[& _]`, not `[]`.

Building the prep module itself does not destroy the prep's output. The
build's clean step wipes only the build's own previous output. The declared
`:ensure` content — including a class dir the prep owns — survives and
lands in the jar.

# Java sources

Rig compiles Java in two situations: when you build a module that declares
`:rig/java-src-dirs`, and automatically, when a module you build depends on
another local module that declares them.

## Your own module

A module with Java sources declares the directories:

```edn
{:rig/java-src-dirs ["java"]}
```

`rig build` then javacs them into the module's class dir **before**
compiling its Clojure, so Clojure code that references the module's own
Java classes compiles. The classes land in the jar, the uberjar and the
native image like anything else compiled into the class dir. Options go in
`:rig/javac-opts`; when the workspace pins `:rig/jvm`, Rig prepends
`--release <n>` unless your opts already set the source level.

## Local dependency modules

The situation this exists for: module `b` depends on module `a` by
`:local/root`, and `a`'s usable output is compiled Java (generated protobuf
classes, JNI bindings, …). The consumer cannot compile until `a`'s classes
exist.

Two declarations make it work, both in `a`:

```edn
;; a/deps.edn
{:paths            ["src" "resources" "target/classes"]  ;; (1)
 :deps             {…}
 :rig/java-src-dirs ["src"]}                             ;; (2)
```

1. `:paths` must include the class dir. Rig flows a local dependency's
   `:paths` into every dependent's classpath — that is how `b` sees
   `a/target/classes`. Without this entry the consumer compiles against an
   empty (or absent) directory.
2. `:rig/java-src-dirs` points at `a`'s Java sources.

With that, `rig build` / `test` / `run` of `b` (or of anything downstream
of `a`) javacs `a` automatically — on `a`'s **locked** base classpath,
into `a`'s class dir — before `b` compiles. The javac is the same one a
full build of `a` runs, and it carries the same staleness tracking as
everything else in Rig: a stamp under `a/target/` records the manifest
hash, a content digest of the sources and a digest of the locked
dependencies; any of them changing (or a dependency of `a` being
re-prepped) re-runs the javac. `rig clean` removes the output and the
stamp.

No re-resolution happens: the classpath the javac uses is the locked one
Rig already fetched and hashed. There is no in-JVM Maven step, so a
dependency's repository credentials are never needed at prep time.

## Migrating away from `:deps/prep-lib`

The common historical shape for "compile the generated Java before anyone
else builds" is a tools.deps prep library:

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

If the prep function is just "javac my own sources into my class dir",
Rig already does that natively. The migration:

1. add `:rig/java-src-dirs` to the proto module (point it at the Java
   sources);
2. keep the class dir in `:paths`;
3. delete `:deps/prep-lib`, the `:prep` alias, the `build.clj`, and the
   tools.build git dependency from the prep classpath;
4. `rig lock` again (the lock records `java-src-dirs` and drops the
   `:prep` alias).

Consumers change nothing — they still depend on the proto module by
`:local/root`, and Rig prepares it for them. The manifest is
self-contained afterwards: no build file Rig never reads, no second
classpath, no separate function whose calling convention you have to
know.

A module that declares **both** `:rig/java-src-dirs` and a prep function
gets the javac first and then its prep function: the function's classpath
leads with the freshly compiled classes. The javac does not clear the
prep's `:ensure` output — a clean wipes only the build's own previous
output. Each keeps its own staleness stamp.

## When to keep `:deps/prep-lib`

Keep the prep library when the preparation is not plain javac: AOT
compiling the module's own Clojure for its dependents, code generation
that must run inside the module's own classpath, anything else custom.
Rig runs the function on the locked `:prep` alias classpath, verifies the
declared `:ensure` output, and tracks staleness exactly as for the native
javac. The function is called with a single `nil` argument — the way
tools.deps' `exec-prep!` invokes it when the alias declares no
`:exec-args` — so declare it `[f]` or `[& _]`, not `[]`.

Building the prep module itself does not destroy the prep's output: the
build's clean step wipes only the build's own previous output, so the
declared `:ensure` content — including a class dir the prep owns —
survives and lands in the jar.

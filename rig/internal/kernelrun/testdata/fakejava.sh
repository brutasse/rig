#!/usr/bin/env bash
# Fake CRaC-capable java for kernelrun tests. Behavior switches (env):
#   FAKE_PLAIN=1      pretend to be a JVM WITHOUT the CRaC flags
#   FAKE_RESTORE=...  ok (default) | nodump | opfail | plainzero | crash | jvmfail
#   FAKE_BOOT=...     ok (default) | fail
#   FAKE_PWD_OUT=path restore mode records its cwd there (workspace anchor)
args="$*"
for a in "$@"; do
    case "$a" in
        -XX:CRaCCheckpointTo=*) TO="${a#*=}" ;;
    esac
done

case "$args" in
    *PrintFlagsFinal*)
        echo 'openjdk version "25.0.4-fake"'
        if [ "${FAKE_PLAIN:-0}" != 1 ]; then
            echo 'ccstr CRaCCheckpointTo = ' >&2
        fi
        exit 0
        ;;
esac

if [[ "$args" == *CRaCRestoreFrom* ]]; then
    [ -n "$FAKE_PWD_OUT" ] && printf '%s' "$PWD" >"$FAKE_PWD_OUT"
    case "${FAKE_RESTORE:-ok}" in
        ok)
            echo '{"ok":true,"via":"restore"}'
            echo '[1234.5s][info][crac] Checkpoint ...'
            echo '7243.697745: warp: Checkpoint successful!'
            mkdir -p "$TO"
            echo pages >"$TO/core.img"
            kill -9 $$
            ;;
        nodump)
            echo '{"ok":true,"via":"restore"}'
            exit 137
            ;;
        opfail)
            # Real kernel protocol: an op failure prints on stderr only
            # (main.clj emits no stdout JSON) and exits 1.
            echo 'boom' >&2
            exit 1
            ;;
        jvmfail)
            # The JVM itself gave up before the op ran: a CRaC restore of
            # an unopenable image exits 1 with the reason on stdout and a
            # generic JVM error on stderr.
            echo '[0.009s][error][crac] Cannot open CRaCRestoreFrom=/nope: No such file or directory'
            echo 'Error: Could not create the Java Virtual Machine.' >&2
            exit 1
            ;;
        plainzero)
            echo '{"ok":true,"via":"restore"}'
            exit 0
            ;;
        crash) kill -11 $$ ;;
    esac
fi

if [[ "$args" == *CRaCCheckpointTo* && "$args" == *rig.kernel.Bootstrap* ]]; then
    case "${FAKE_BOOT:-ok}" in
        ok)
            echo '{"ok":true,"via":"bootstrap"}'
            mkdir -p "$TO"
            echo pages >"$TO/core.img"
            kill -9 $$
            ;;
        fail) exit 1 ;;
    esac
fi

# Cold kernel invocation: java [-flags] -jar jar --request -
# FAKE_STDIN_OUT=path records the request JSON (protocol assertions).
if [ -n "${FAKE_STDIN_OUT:-}" ]; then cat >"$FAKE_STDIN_OUT"; else cat >/dev/null; fi
echo '{"ok":true,"via":"cold"}'
exit 0

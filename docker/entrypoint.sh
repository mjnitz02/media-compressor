#!/bin/sh
# Entrypoint for the media-compressor container.
#
# It does four small things and then gets out of the way: work out which uid to
# run the program as, leave a readable example config where the operator will
# look for it, say something useful if there is no config yet, and warn if the
# GPU was not passed through.
set -eu

CONFIG_DIR=/config
CONFIG=$CONFIG_DIR/config.yaml
WORK_DIR=/temp
EXAMPLE=/usr/share/media-compressor/config.example.yaml
BIN=/usr/local/bin/media-compressor

# Refreshed on every start so that after an upgrade the example next to the
# real config documents the schema this binary actually understands. The
# operator's file is config.yaml; this one is never read by the program.
if [ -d "$CONFIG_DIR" ]; then
    cp -f "$EXAMPLE" "$CONFIG_DIR/config.example.yaml" 2>/dev/null || true
fi

# `version` and `help` answer without reading anything, so they must keep
# working in a container that has not been configured yet -- which is exactly
# the container somebody is poking at when they run them.
case "${1:-}" in
    version|help|-h|--help|"")
        exec "$BIN" "$@"
        ;;
esac

# --- PUID / PGID -------------------------------------------------------
#
# On a NAS every share is typically owned by one account -- 99:100 on Unraid --
# and a container that writes as root leaves files the rest of the box cannot
# manage. So the standard PUID/PGID pair is honoured here.
#
# It matters more for this program than for most, because safety rule 11 gives
# every replacement the original file's owner. Running as the uid that already
# owns the library turns that chown into a same-owner call, which the kernel
# permits for the file's owner -- so the rule keeps working without CAP_CHOWN,
# and the "could not set owner" note now means something real: a file owned by
# somebody unexpected.
#
# Unset means stay root, which is what this image did before PUID existed and
# what a plain `docker run` with no NAS conventions still wants.
#
# Everything here only prepares the drop. The exec itself is the last line of
# the file, so that the checks below still run -- and still have root's
# unrestricted read -- before anything is handed over.
privdrop=""
if [ -n "${PUID:-}" ] || [ -n "${PGID:-}" ]; then
    PUID=${PUID:-0}
    PGID=${PGID:-$PUID}

    if [ "$(id -u)" != 0 ]; then
        # --user was passed as well. Nothing to drop from, and silently
        # ignoring one of two conflicting instructions is worse than saying so.
        echo "media-compressor: warning: PUID/PGID were set but this container is" >&2
        echo "  already running as $(id -u):$(id -g) (--user, or a user: in compose)." >&2
        echo "  PUID=$PUID PGID=$PGID ignored; --user wins." >&2
    elif [ "$PUID" != 0 ]; then
        # Reuse whatever already holds the id where one does, so the names in
        # `ps` and `id` match the host's. Failing to create them is not fatal:
        # setpriv works on the numbers, and a missing passwd entry costs
        # nothing this program needs.
        getent group "$PGID" >/dev/null 2>&1 || groupadd -g "$PGID" mediacompressor 2>/dev/null || true
        getent passwd "$PUID" >/dev/null 2>&1 || useradd -u "$PUID" -g "$PGID" -M -N -s /usr/sbin/nologin mediacompressor 2>/dev/null || true

        # The two directories this image declares as volumes are the only paths
        # it may take ownership of. Media is emphatically not on that list: it
        # belongs to the operator, this tool is a guest in it, and chowning a
        # library would be exactly the kind of unasked-for change the safety
        # rules exist to prevent.
        for dir in "$CONFIG_DIR" "$WORK_DIR"; do
            [ -d "$dir" ] || continue
            chown -R "$PUID:$PGID" "$dir" 2>/dev/null ||
                echo "media-compressor: warning: could not chown $dir to $PUID:$PGID" >&2
        done

        # Supplementary groups docker was told to add (`group_add:` in compose,
        # --group-add) arrive on this process and would be thrown away by a
        # plain --clear-groups. They are how a render node that is not
        # world-readable gets reached, so carry them across the drop. Root's own
        # gid 0 is not one of them, and neither is the gid being set.
        extra=$(id -G | tr ' ' '\n' | grep -vx -e 0 -e "$PGID" | paste -sd, - || true)
        if [ -n "$extra" ]; then
            privdrop="setpriv --reuid $PUID --regid $PGID --groups $extra --"
        else
            privdrop="setpriv --reuid $PUID --regid $PGID --clear-groups --"
        fi

        # For anything the operator later runs with `docker exec`; the program
        # itself reads neither.
        export HOME=$CONFIG_DIR USER=mediacompressor
    fi
fi

if [ -n "${UMASK:-}" ]; then
    umask "$UMASK"
fi

if [ ! -f "$CONFIG" ]; then
    cat >&2 <<MSG
media-compressor: no configuration at $CONFIG

A copy of the documented example is now at $CONFIG_DIR/config.example.yaml.
Copy it to config.yaml, point its libraries at the paths you mounted, and
start the container again.

  media-compressor validate     reads it back and says what each library gets
  media-compressor plan         prints what would happen, and touches nothing

Nothing was read and nothing was written.
MSG
    exit 1
fi

# A hardware encoder with no render node fails on every single file, one at a
# time, hours apart. Worth one line at startup. Not fatal: the config may name
# libx265, and refusing to start would be this tool deciding it knows better
# than the file it was given.
if [ ! -d /dev/dri ]; then
    echo "media-compressor: warning: /dev/dri is not present in this container." >&2
    echo "  hevc_vaapi and hevc_qsv cannot work without it -- pass the GPU through" >&2
    echo "  with --device /dev/dri, or set a software encoder in config.yaml." >&2
fi

# Unquoted on purpose: $privdrop is either empty (stay as we are) or the setpriv
# command and its flags, which must split into words here.
# shellcheck disable=SC2086
exec $privdrop "$BIN" "$@"

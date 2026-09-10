#!/bin/sh
# Entrypoint for the media-compressor container.
#
# It does three small things and then gets out of the way: leave a readable
# example config where the operator will look for it, say something useful if
# there is no config yet, and warn if the GPU was not passed through.
set -eu

CONFIG_DIR=/config
CONFIG=$CONFIG_DIR/config.yaml
EXAMPLE=/usr/share/media-compressor/config.example.yaml

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
        exec /usr/local/bin/media-compressor "$@"
        ;;
esac

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

exec /usr/local/bin/media-compressor "$@"

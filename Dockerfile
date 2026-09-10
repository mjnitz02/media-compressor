# syntax=docker/dockerfile:1

# media-compressor: one static Go binary plus an ffmpeg that can talk to an
# Intel GPU. Nothing else belongs in here.
#
# The runtime stage is Debian rather than Alpine because the thing being
# packaged is not the binary -- that is 14 MB and needs no libc at all -- it is
# jellyfin-ffmpeg, which is a Debian package. See docs/plan.md, Phase 6.

# --- build -------------------------------------------------------------
#
# Pinned to the toolchain in go.mod. --platform=$BUILDPLATFORM keeps this
# stage native on the build machine and cross-compiles instead of running the
# whole Go toolchain under emulation, which is the difference between one
# minute and twenty on an ARM laptop.
FROM --platform=$BUILDPLATFORM golang:1.27-bookworm AS build

WORKDIR /src

# Dependencies first: a source-only change should not re-download them.
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

COPY . .

ARG TARGETARCH
ARG VERSION=dev

# CGO off because the SQLite driver is modernc.org/sqlite, which is pure Go.
# That is what makes this one file with no runtime linkage to worry about.
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=linux GOARCH="$TARGETARCH" \
    go build -trimpath \
      -ldflags "-s -w -X main.version=${VERSION}" \
      -o /out/media-compressor ./cmd/media-compressor

# --- runtime -----------------------------------------------------------
FROM debian:bookworm-slim

# jellyfin-ffmpeg is used because it is maintained specifically for hardware
# transcoding and ships the whole Intel stack inside its own tree:
# lib/dri/iHD_drv_video.so, libva, libvpl and libmfx-gen. So there is no
# non-free apt component to enable and no intel-media-va-driver to keep in
# step with it -- the driver and the ffmpeg that uses it move together.
#
# Pinned by version and checksum rather than pulled from the apt repo: an
# upgrade of the one component that touches the GPU should be a commit that
# says so, not something a rebuild does quietly.
ARG FFMPEG_VERSION=7.1.4-3-bookworm
ARG FFMPEG_SHA256=0cf62bc2423822c9ec7a38dbc8f526d9a58671bd01843daa7817ece35619fc1c

ARG TARGETARCH
RUN set -eu; \
    # QSV and VAAPI are Intel x86 only, and this image exists for a machine
    # with an Intel GPU in it. Failing here is better than shipping an arm64
    # image that implies hardware encoding it cannot do.
    if [ "$TARGETARCH" != "amd64" ]; then \
      echo "media-compressor is built for linux/amd64 only (got ${TARGETARCH}): the encoders it exists to drive are Intel QSV/VAAPI" >&2; \
      exit 1; \
    fi; \
    apt-get update; \
    apt-get install -y --no-install-recommends \
      ca-certificates \
      curl \
      tzdata; \
    curl -fsSL -o /tmp/jellyfin-ffmpeg.deb \
      "https://repo.jellyfin.org/debian/pool/main/j/jellyfin-ffmpeg/jellyfin-ffmpeg7_${FFMPEG_VERSION}_amd64.deb"; \
    echo "${FFMPEG_SHA256}  /tmp/jellyfin-ffmpeg.deb" | sha256sum -c -; \
    apt-get install -y --no-install-recommends /tmp/jellyfin-ffmpeg.deb; \
    rm -f /tmp/jellyfin-ffmpeg.deb; \
    rm -rf /var/lib/apt/lists/*; \
    # The package deliberately keeps itself off PATH so it cannot collide with
    # a distro ffmpeg. Nothing else here provides one, so put it on PATH and
    # the -ffmpeg / -ffprobe flags keep their defaults.
    ln -s /usr/lib/jellyfin-ffmpeg/ffmpeg  /usr/local/bin/ffmpeg; \
    ln -s /usr/lib/jellyfin-ffmpeg/ffprobe /usr/local/bin/ffprobe; \
    ln -s /usr/lib/jellyfin-ffmpeg/vainfo  /usr/local/bin/vainfo

COPY --from=build /out/media-compressor /usr/local/bin/media-compressor
COPY config.example.yaml /usr/share/media-compressor/config.example.yaml
COPY docker/entrypoint.sh /usr/local/bin/entrypoint.sh

# /config holds config.yaml and the SQLite database; /temp is scratch space
# for in-progress encodes. These are the paths config.example.yaml already
# names, so the shipped example works unedited apart from the library paths.
#
# Media is mounted read-write at whatever paths the config names -- this image
# declares nothing about it, because where the media lives is the operator's
# business and one of the safety rules is that this tool never relocates it.
RUN mkdir -p /config /temp
VOLUME ["/config", "/temp"]

EXPOSE 8080

# Says the process is up and serving, which is not the same as saying a pass
# is making progress -- that question is what the History and Held back pages
# answer. Do not read more into a green dot than that.
HEALTHCHECK --interval=60s --timeout=5s --start-period=10s --retries=3 \
  CMD curl -fsS http://127.0.0.1:8080/healthz || exit 1

# Root, deliberately. Safety rule 11 gives every replacement the original
# file's owner, and chown to an arbitrary uid needs CAP_CHOWN. Running this
# with --user works and is safe -- the encode is still verified before the
# replace -- but replaced files take this process's uid instead, and the run
# records a note saying it could not set the owner.
ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]
CMD ["daemon"]

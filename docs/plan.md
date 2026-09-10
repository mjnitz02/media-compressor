# Build plan

Phases are ordered so that the highest-value, lowest-infrastructure work comes
first. Each phase ends somewhere you could stop and still have something
useful.

## The governing principle: doing nothing is a valid result

This is an optimiser, and it acting on any given file is never a given. The old
stack had a bail-out at every step, and that is a feature, not an accident of
its design:

- Already HEVC? Leave it.
- Bitrate too low to cut without visible loss? Leave it.
- Any chance of losing a track that mattered? Leave it.
- A stream type we do not recognise? Keep it, and leave the file alone.

The reason is that the thing being optimised is not disk space, it is a media
library other people watch, largely unattended. The cost of over-compressing or
pruning the wrong track is someone reporting that a film is broken, and a
source file that no longer exists to go back to. The cost of declining is a few
gigabytes. Those are not comparable, so every ambiguous case resolves toward
leaving the file be.

`internal/decide` reflects this directly: `ActionNone` is the most common
outcome by design, every decline records a human-readable reason, and a safety
rule overriding the configuration is recorded in `Plan.Notes` rather than
applied silently.

## Operating assumptions

Recorded because they shaped the design and are not visible from the code:

- **It runs unattended.** The old install was opened roughly twice a year. So
  errors must be visible long after the fact, and the tool must never wedge
  itself or need a nudge to make progress.
- **Files arrive by themselves,** from Sonarr, Radarr, other \*arr stacks, or
  by hand. New arrivals join the queue; there is no manual submission step.
- **Files are not settled when they first appear.** This is an Unraid server
  with a lot of moving parts: a file may shift between shares after landing, or
  appear as a temp file while the real one is still being fetched. Hence a
  grace period before a file is eligible, and a size-stability check on top of
  age (Phase 4).
- **Encode-then-replace-in-place is load-bearing.** Plex will swap a file
  underneath a running playback and it works well in practice, but only while
  the name is identical. That, plus the \*arr stacks tracking files by path, is
  why nothing is ever renamed. See the container note in Phase 2.

## Why Go

The whole job is subprocess orchestration: run ffprobe, parse JSON, decide, run
ffmpeg, move a file, serve a page. That is `os/exec`, `encoding/json`,
`net/http`, `html/template` — all standard library.

Concretely, over the alternatives:

- **One static binary.** The Dockerfile's second stage copies in a single file.
  No venv, no pip resolution at build time, no runtime interpreter.
- **`embed.FS`** compiles the UI into the binary. No static-assets volume, no
  frontend build step, nothing to serve from disk.
- **Goroutines and channels** make the two-pool worker model (below) about
  fifteen lines.

The learning curve to expect: explicit `if err != nil` error handling and no
exceptions. Mildly tedious for a week, then unremarkable. This project touches
almost none of Go's genuinely awkward corners.

---

## Phase 0 — Golden corpus ✅ done

`tools/extract_tdarr_corpus.py` → `testdata/tdarr_decisions.jsonl.gz`.

1,881 deduplicated `(ffprobe input → Tdarr decision)` fixtures from the real
library: 161 transcodes (158 with the exact ffmpeg command), 203 bitrate-floor
skips, 1,517 already-done. 857 KB compressed.

This was extracted first because the Tdarr appdata it came from is a temporary
directory that will be deleted.

## Phase 1 — `probe` + `decide` ✅ done

The core of the project, and pure Go with no infrastructure.

- `internal/probe` — ffprobe types plus the one function that runs it.
  `Prober.Run` is the only part that touches the world.
- `internal/decide` — **no I/O whatsoever.** Takes a probe result and a
  profile, returns a `Plan`. No filesystem, no subprocess, no clock.

**Definition of done: met.** `TestGoldenCorpus` runs `Decide` over all 1,881
fixtures and matches the old stack on every one — the video decision, the
bitrate arithmetic to the kbps, and the **exact ffmpeg argument list, token for
token, on all 1,877 fixtures that recorded one.**

Matching the commands exactly meant reproducing the old plugin's genuine bugs,
which is what `decide.Quirks` is for. Each quirk is one named, documented field,
so `TdarrCompat()` and `Standard()` differ only in ways you can read off in one
place.

### What `Standard` changes, measured on the corpus

`TestStandardDivergesFromTdarrOnlyWhereIntended` runs both profiles over the
same 1,881 files and pins the difference, so nothing drifts unnoticed:

| Change | Files |
|---|---|
| No longer an encode candidate (28 AV1, 12 with PNG cover art) | 40 |
| Keeps more subtitle tracks (forced-subtitle rule) | 52 |
| **Loses an audio track the old stack kept** | **0** |
| Newly re-encoded | **0** |

The last two rows are the ones that matter. `Standard` is never less
conservative about re-encoding than the stack it replaces, and it never drops
audio the old stack kept.

### Bugs found in the old stack, and what was done about them

Found by making the corpus pass, and each now covered by a test:

1. **PNG cover art was treated as a video stream.** The plugin recognised only
   MJPEG as artwork. Twelve corpus files are HEVC videos routed into the encode
   branch because of an attached PNG poster; only the bitrate floor stopped
   them being re-encoded. `probe.MainVideo` now skips anything with an
   `attached_pic` disposition or a still-image codec.
2. **AV1 was an encode candidate.** AV1 is more efficient than HEVC, so
   transcoding it down loses quality and space. All 28 AV1 files in the corpus
   escaped only because they fell below the floor. `Standard` never re-encodes
   AV1.
3. **A language keep-list can leave a file silent.** Two corpus files have a
   single audio track tagged `chi` and `aze`. Applying the keep-list literally
   produces a video with no audio at all — unwatchable, and unrecoverable once
   the source is deleted. `buildAudio` now refuses to drop the last audio
   track, and records a note saying it overrode the config.
4. **A duration constant of `0.0166667` instead of `1/60`.** Slightly lowers
   every computed bitrate. Under a tenth of a percent, but enough to change the
   integer at a truncation boundary, so it is preserved as a quirk and
   corrected in `Standard`.

### New rules, from how the old setup was actually operated

5. **Language tags are only acted on when they look credible.**
   `PrimaryLanguages` names the languages a library would be expected to
   contain. If a file has no track in any of them, the tags are treated as
   unreliable and nothing is pruned on language. This is the anime case
   directly: a release with a Korean track and no Japanese one is far more
   likely mistagged than genuinely unwanted.
6. **Forced subtitles outrank the language list.** A forced track is the
   translated-signage track for a film watched in its original audio, so losing
   one is very visible.
7. **Untagged tracks get labelled** (`TagUntaggedAs`) so a player's track
   picker shows something better than "Unknown". Note that enabling this makes
   otherwise-untouched files need one cheap, lossless remux pass.

## Phase 2 — Config

- `internal/config` — parse `config.yaml`, resolve `extends`, apply
  `defaults`, validate paths exist and profiles resolve.
- Fail loudly and specifically at startup. A typo'd profile name must be a
  startup error, never a silent fallback to different encode settings.
- `mediacompressor validate` subcommand.

### The container toggle

`container` is one of `mkv`, `mp4`, or `source`, set per profile and
overridable per library. It is implemented in `decide` already; Phase 2 is
about exposing it.

The point of the setting is **not** to convert libraries into MP4. It is to be
able to say "don't force a container change." A library holding a mix of MKV
and MP4 has files that are perfectly fine as they are; when one of them turns
out to be a viable encode candidate on bitrate, it should be re-encoded to HEVC
and left in whatever container it was already in.

- **`source`** — keep whatever container the file already has, whether or not
  it is also being re-encoded. This is the setting for a mixed library, and for
  anything not under Plex streaming where standardising on MKV buys nothing.
  Its only extension change is the fallback to MKV for containers this tool
  does not write (avi, wmv, vob).
- **`mkv`** — always Matroska, which is what the old stack did. The default,
  and the right choice for the main Plex libraries.
- **`mp4`** — always MP4. Rarely what you want: it renames every MKV in the
  library and costs streams (below). It exists for a library you specifically
  want in that shape.

#### Keeping a container forces nothing

A stream sitting in a container is proof that the container can hold it. So a
file that keeps its own container never has a codec change forced on it, and
never loses a stream. `constraintsFor` implements exactly this: container rules
apply only when the container is actually changing.

This was a real bug in the first cut of the code, and it is worth recording
because the reasoning is not obvious. FLAC in MP4 is legal and plays fine, so
an existing MP4 can contain it. Applying "MP4 cannot carry FLAC" to a file that
was *staying* in MP4 meant re-encoding that track to AAC — discarding quality
for no reason whatsoever. `TestKeepingTheContainerForcesNothing` pins it.

The consequence is a clean rule: **only forcing a container can cost you a
stream.** Under `source`, nothing is ever dropped or converted for container
reasons at all.

#### What forcing MP4 actually costs

Enforced in code, and each instance recorded in `Plan.Drops` or `Plan.Changes`
with a reason:

- Image-based subtitles (PGS, VOBSUB, DVB, teletext) are **dropped**. There is
  no MP4 representation short of burning them into the video, which this tool
  will not do.
- Text subtitles are rewritten as `mov_text`, which keeps the dialogue but
  discards ASS positioning, fonts and karaoke styling. Reason enough to leave
  anime libraries on MKV or `source`.
- Audio MP4 cannot hold (TrueHD, Vorbis, PCM) is converted.

#### Renaming, and why it needs deciding per library

Changing container changes the extension, and that is the only rename
permitted anywhere in this project — same directory, same base name, new
extension.

It is not free. An identical filename is exactly what lets Plex swap a file
underneath a running playback, and the \*arr stacks track files by path. So
`mkv` pointed at a library of MP4s will rename every file in it.
`Plan.ExtensionChanges()` reports the case so `--dry-run` can call it out
before anything happens.

## Phase 3 — `encode`, and the safety rules

The phase where mistakes are expensive. Implement the rules in README.md
literally:

1. Encode to a temp file, preferring `work_dir`, falling back to the source
   directory when `work_dir` is on a different filesystem (so the final
   `rename()` stays atomic).
2. Verify: ffprobe parses, duration within ~1s of source, expected stream
   count present, size not implausibly small.
3. `rename()` over the original only after verification passes.
4. Never delete the source by any other path.
5. On any failure, leave the original untouched and keep the temp file for
   inspection.

`--dry-run` lands here as a first-class mode, printing the full plan for a
library and touching nothing. Given that the old stack skipped 7,880 files, the
first question about any run is "what will this actually touch?" — that must be
answerable without risk.

Tests use tiny generated clips (a few frames of `testsrc`), not library files.

## Phase 4 — Scanner, store, queue

- `internal/store` — SQLite. One table of file state keyed on
  `(path, size, mtime)` so a rescan doesn't re-probe 20k unchanged files, plus
  a job history table.
- Walk configured roots, honour `ignore_globs` and `min_age_seconds`.
- **Age alone is not enough.** A file can be older than the grace period and
  still be mid-move on a server with this many moving parts, so eligibility is
  age *and* a size that has not changed between two consecutive scans.
- **Two worker pools.** Remux/cleanup work is I/O bound and can run several
  at once; hardware HEVC encoding is bottlenecked on one iGPU and realistically
  supports 1–2. Conflating these is part of why Tdarr's scheduling felt
  arbitrary.
- Config lives in YAML, state lives in SQLite, and the two never mix. Reading
  the old Tdarr configuration required cracking open a SQLite blob; that is the
  anti-pattern being avoided.

## Phase 5 — Web UI

Read-mostly, deliberately minimal: what's queued, what's running with progress,
what was skipped and why, recent history, and a button to trigger a scan.

- Go `html/template` + `embed.FS`, HTMX (one vendored file) for polling.
- **No SPA, no node, no build step.** A queue view does not need one, and
  adding one doubles both the container and the build.
- The "skipped and why" view is the most valuable screen — it is how you
  audit that the decision engine is behaving. It also has to make sense to
  someone who last looked six months ago, which means every skip carries the
  reason string `decide` produced, not a status code.
- `Plan.Notes` gets its own view: a note means a safety rule overrode the
  configuration, which is the one thing worth reading proactively.

## Phase 6 — Docker + GHCR

- Multi-stage: `golang` build stage → runtime stage on `jellyfin-ffmpeg`
  (maintained specifically for Intel QSV/VAAPI hardware transcoding).
- Expect **~400–500 MB**. The Intel media driver stack is the bulk and cannot
  be avoided. That is ~4x smaller than Tdarr, and every megabyte is accounted
  for.
- Requires `/dev/dri` passthrough.
- GitHub Actions → GHCR on tag. Unraid template in `unraid/`.
- Appdata layout: `/config` (config.yaml + SQLite), `/temp` (work dir),
  media mounted read-write.

---

## Deferred, deliberately

Revisit only once phases 1–6 are running in production:

- **Switching encoder to `hevc_qsv` with ICQ, or x265 CRF.** Quality-targeted
  rate control gives consistent quality per scene instead of a bitrate floor
  that overspends on easy content. Likely equal-or-better perceptual quality at
  60–70% of current output size, which would also make the 3000 kbps floor
  droppable — and the floor is currently excluding thousands of files. Needs
  VMAF comparison against current output on a sample before committing.
- Plex refresh hooks (per-library, off by default, never for `isolated`).
- Health-check / corruption-scan pass.

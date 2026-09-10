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

## Phase 2 — Config ✅ done

`internal/config` parses `config.yaml`, resolves `extends`, applies defaults
and validates the result, and `media-compressor validate` prints what each
library will actually get. 98.3% covered.

**Definition of done: met.** The binding test is
`TestExampleConfigReproducesTheBuiltinProfiles`: the `standard` and `anime`
profiles in `config.example.yaml` must parse into exactly `decide.Standard()`
and `decide.Anime()`, which are the profiles the golden corpus was verified
against. The file an operator starts from therefore cannot drift away from the
behaviour that was measured, and if someone changes one, the test says which
field and by how much.

### How `extends` merges

A child profile is built by decoding the parent's YAML into a struct and then
decoding the child's YAML into that same struct. yaml.v3 only writes the fields
a document actually mentions, so a child overrides exactly what it names and
inherits everything else — including sibling fields inside a block it partly
overrides.

Lists replace rather than append. `keep_languages: [eng]` in a child means
"only English"; if it appended, a profile could never narrow anything.

### Container precedence

Library, then profile, then `defaults`. All three are checked, and the library
override is applied to a copy so that two libraries sharing a profile cannot
affect each other.

### What is deliberately not configurable

- **`Quirks`.** They are bug-for-bug switches for the stack being replaced, so
  enabling one in production would mean asking for a known bug. Every parsed
  profile gets the corrected values; `quirks:` in a config file is an unknown
  field and therefore an error.
- **`KeepAllLanguages`**, part of the same compatibility shim.
- **`-bufsize`**, which is always the source bitrate. The draft schema had a
  `bufsize_basis` key with exactly one legal value; a knob that can only be in
  one position is noise, so it was removed rather than implemented.

`leave_alone` went the other way — it was missing from the draft schema and is
now explicit, because "which codecs are never re-encoded" is a real quality
lever and AV1 being in that list is one of the four corrected bugs.

### Validation, and the question it asks

Every check earns its place by answering: *if this were wrong, when would you
find out?* A misspelled profile name fails at startup, which is fine. These do
not fail at all on their own:

| Check | What it prevents |
|---|---|
| `floor_kbps` must be written down, even as `0` | A forgotten key defaulting to no floor sends several thousand files into an encode the old stack declined |
| Bitrate tiers ordered highest-first, ending at `above: 0` | The engine takes the first tier a bitrate reaches, so a mis-ordered table silently applies the wrong divisor |
| `leave_alone` contains the target codec | Otherwise every converted file is a candidate again on the next scan, forever |
| Language codes look like language codes | `englsh` does not error, it just stops matching — and you find out months later when a film has no English audio |
| `primary_languages` ⊆ `keep_languages` | A profile that names a language as the one the library is built around while also dropping it |
| No two library paths overlap | One file with two profiles, resolved by scan order |
| Divisor ≥ 1 | A divisor below 1 raises the bitrate above the source |

All problems are reported together. The operator opens this file about twice a
year; fixing it one error per run is miserable.

Path existence is checked separately (`CheckPaths`, and
`validate -check-paths=false`) so that a server's config can be validated from a
laptop where none of the mounts exist. Parsing itself touches no filesystem.

**Gap recorded here, closed in Phase 3:** `path/filepath.Match` does not
understand `**`, so `**/.Recycle.Bin/**` cannot match nested paths with the
standard library alone. `scan.Match` implements it, and validation now uses
that same matcher — so the syntax accepted at startup is the syntax that will
actually fire.

### The container toggle

`container` is one of `mkv`, `mp4`, or `source`, set in `defaults`, overridable
per profile, and overridable again per library. `decide` implements it and the
config exposes it.

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

## Phase 3 — `encode`, and the safety rules ✅ done

`internal/encode` runs a plan, and `media-compressor plan` / `run` drive it
over a library or a list of paths. `internal/scan` finds the candidate files.

**Definition of done: met.** All five rules are implemented and each has a test
that proves the original survives when the rule fires:

1. Encode to a temp file, preferring `work_dir`, falling back to the source
   directory when `work_dir` is on a different filesystem.
2. Verify: ffprobe parses, duration within ~1s, expected streams present, size
   plausible.
3. `rename()` over the original only after verification passes.
4. Never delete the source by any other path.
5. On any failure, leave the original untouched and keep the temp file.

### Prepare and Run, and why `--dry-run` is not a simulation

`Encoder.Prepare` works out every path and the complete ffmpeg argument list
and touches nothing. `Encoder.Run` executes exactly that. `--dry-run` is
`Prepare` without `Run`, so what it prints is the `Job` that would have been
executed rather than a description of one — there is no second code path that
could drift.

### The temp file

The rule is entirely about the final step being a `rename()`. A rename is
atomic; across filesystems it is not a rename at all but a copy, which is
neither atomic nor free. So `work_dir` is used only when it is on the same
filesystem as the file being replaced, and otherwise the temp file goes beside
the source. On the real server, where `/temp` is cache and the media is on the
array, that means it will usually go beside the source.

Its name is `.<base>.<hash8>.mediacompressor.partial<ext>`, which is three
requirements at once: the extension is how ffmpeg picks a muxer, the leading
dot and `.partial.` mean a leftover from a crashed run is hidden *and* caught
by the shipped `ignore_globs`, and the path hash keeps two `S01E01.mkv` from
colliding in a shared work dir. `TestShippedGlobsCatchOurOwnTempFiles` pins the
second one — a temp file the next scan picks up as a new arrival would be a
genuinely nasty loop.

### Verification

Every check is pure arithmetic over two probe results and a plan, so it is
testable without ffmpeg; the only I/O is the caller's ffprobe.

| Check | What it catches |
|---|---|
| Output ffprobes cleanly | A file that is not media at all |
| Duration within 1s | A truncated encode — ffmpeg killed part-way still writes a valid file and can still exit 0 |
| Source duration is known | An output that cannot be compared to anything cannot be verified, so it cannot replace anything |
| Stream counts match the plan | ffmpeg quietly declining to carry a stream: a warning on stderr, exit 0, and a file that plays perfectly minus one track |
| Main video is the target codec (encodes only) | An encoder falling back to something else, which would then be re-encoded on every scan forever |
| Output ≥ 10% of the source | A header and nothing else |
| A re-encode did not grow the file | An hour of GPU time spent making the file worse, then deleting the better original |

The last one is the only check that is about the point of the exercise rather
than about safety. It is off with `VerifyOptions.AllowLargerOutput`, and it is
on by default because there is no version of that outcome worth keeping.

### Three rules added while building it

Each is a real property of this server rather than a general principle, which
is why none of them were in the original list:

- **A symlink is refused.** Renaming over the link would leave a regular file
  at that path and orphan the real file wherever it lives — a quiet
  reorganisation of the library, which rule 1 forbids. `scan` keeps symlinks
  out of the candidate list and `Prepare` refuses them again.
- **The source is re-stat'd immediately before the rename.** Files arrive by
  themselves here. An \*arr stack upgrading the source mid-encode would
  otherwise have its new file silently overwritten by a re-encode of the old
  one, so size and mtime are recorded before the encode and checked after it.
- **The replacement adopts the original's mode and owner.** A rename gives the
  new file this process's umask and uid, not the library's. On a share read by
  Plex, the \*arr stacks and SMB alike, a file that turns up as `0600
  root:root` is a support call. Chown failing when not running as root is a
  note, not an error.

A **hard link count above 1** is reported but not refused. The \*arr stacks
hardlink from the download directory routinely; replacing the library path is
correct, it just does not reclaim the space until the other link goes.

### Renaming into an existing file

`Prepare` refuses to convert `Film.mp4` when a `Film.mkv` already exists.
Whatever that file is, it is somebody's.

### `internal/scan`

Walks the roots, filters on extension, applies `ignore_globs` and
`min_age_seconds`, and reports what it declined and why. `Match` implements
`**` — the known gap recorded in Phase 2 — by splitting pattern and path into
segments and letting `**` consume zero or more of them, with `filepath.Match`
doing one segment at a time. `config` now validates globs with the same
matcher, so the syntax accepted at startup is the syntax that will work.

A pattern with no `/` in it matches the base name, which is what anyone
writing `*.partial.*` means.

Eligibility is still only half-implemented on purpose: age is here, and the
size-stability half needs the store, so it arrives in Phase 4.

### The commands

```
media-compressor plan -library movies        # touches nothing
media-compressor run  -library movies -limit 5
media-compressor plan /mnt/media_video/movies/Some.Film.mkv
```

A path is matched back to its library — and therefore to its profile — by
`Config.LibraryFor`. A path inside no library is an error rather than a guess,
because guessing means encoding a folder to some other library's quality
target.

The summary leads with counts by action, then lists every file that would be
touched, then the renames, then `Plan.Notes`. Only a file that could not be
read or whose encode went wrong sets a non-zero exit status; a file this tool
*declines* is reported and moves on, because declining is a valid result and a
library with one odd file in it should not make every run look like a failure.

**Not built here, on purpose:** the worker pools. `run` processes one file at a
time. Splitting remux from encode work is a scheduling concern that belongs
with the queue in Phase 4.

## Phase 4 — Scanner, store, queue ✅ done

`internal/store` (SQLite), `internal/queue` (the two pools) and
`internal/runner` (the thing that ties scan → decide → work together), plus
the `scan`, `status` and `daemon` commands.

**Definition of done: met.** A file arriving in a library is picked up on its
own, waited on until it has stopped moving, decided once, worked on, and
recorded — and a second scan of an unchanged library probes nothing at all.

### The three modes are one code path

```
plan    decide and report. Writes nothing, anywhere — not to the media, and
        not to the database either.
scan    decide and remember. Touches no media.
run     scan, and then do the work.
daemon  run, on the configured interval, until stopped.
```

`plan` not writing to the database is the part worth stating. A dry run that
quietly recorded sightings would make `run` after `plan` behave differently
from `run` alone, and then `--dry-run` would no longer be telling the truth
about anything. It reads the store and writes none of it, and it will not
create the database file if there is not one yet.

### Eligibility: age is not enough, and this is why

A file is worked on only once all three hold:

- its mtime is at least `min_age_seconds` old;
- at least two separate scans have seen it;
- and its size and mtime have been unchanged for at least `min_age_seconds`.

The middle one is the one that is easy to talk yourself out of. Files arrive
on this server by themselves and then keep moving, and a share-to-share move
or an `rsync -t` **preserves the mtime** — so a file can be hours old by its
own timestamp while its bytes are still being written. Age says it is ready;
it is not. The only reliable evidence that a file is finished is that a later
look found it exactly as an earlier one did.

The consequence is worth knowing before it surprises you: **on a brand new
database, nothing is eligible until a second scan has run.** That is the rule
working. `plan` ignores it and shows the whole library regardless, flagging
which files are not eligible yet, so the first thing you run still tells you
everything.

`-unsettled` overrides it, and naming a path explicitly waives it — somebody
typed that path, so they are not waiting for the file to finish arriving.

### The decision cache, and the profile fingerprint

`files` is keyed on path, and holds the last decision alongside the `(size,
mtime)` it was computed from. A changed file discards the decision, the
failure history and the settle clock together — half-resetting them is how a
stale "nothing to do" outlives the file it was about.

The other half of the cache key is a hash of the whole profile. Without it,
editing `floor_kbps` in config.yaml would leave 20,000 cached "nothing to do"
answers in place and the change would appear to do nothing at all — the worst
kind of bug, because it looks like the tool working.
`TestProfileFingerprintTracksEveryQualityLever` pins that every quality lever
moves the hash.

The cache only ever short-circuits **"there is nothing to do"**. Anything with
work in it is re-probed, because the ffmpeg arguments have to be derived from
the file as it is right now. That is not a limitation in practice: on the
corpus, 1,517 of 1,881 files need nothing, and those are the ones that would
otherwise be re-probed every three hours forever.

### Failures back off, and then stop

A file that fails for a reason inherent to the file — an encode that comes out
bigger than the source, an `mkv` that is not actually media — fails identically
on every scan, forever. Unattended, that is a few thousand copies of the same
error and a permanently non-zero exit status, which trains you to ignore the
exit status.

So a failure is recorded against the exact `(path, size, mtime)` and retried
after 1 hour, then 6, then 24, and then not on its own. `status` lists what is
being held back and why; `run -retry-failed` clears it. Changing the file
clears it too, because then it is a different file.

This closes the open question from Phase 3: the "output was larger than the
source" check can now fire on a real file without that file failing on every
run for the rest of time.

### Two pools

`internal/queue` runs remuxes and encodes on separate pools, sized separately.
The property that matters is `TestRemuxesDoNotWaitBehindALongEncode`: a
hundred second-long remuxes must not sit behind one four-hour encode. It does
no I/O of its own — it is handed a function and decides only what runs when —
so the scheduling is testable in milliseconds with ffmpeg nowhere near it.

`Encoder` is a value type, so each worker takes its own copy and hangs its own
progress callback on it. Sharing one callback across a pool would interleave
reports from several files with no way to tell them apart.

### Sweeping, and when not to

A scan forgets rows for files it no longer finds — **unless any directory
failed to read**. A mount that dropped out for a moment looks exactly like a
library somebody deleted, and forgetting 20,000 files because of a transient
hiccup means re-probing all of them and waiting two more scans before any of
them can be touched again. A pass over paths somebody named never sweeps at
all.

### The dependency

`modernc.org/sqlite`, the pure-Go driver, reached for over `mattn/go-sqlite3`
so the binary still builds with `CGO_ENABLED=0` and the container's second
stage stays a single copied file. It sits behind `database/sql`, so swapping
it is a one-line change if that ever becomes necessary.

One connection (`SetMaxOpenConns(1)`). The writes are tiny and rare — a scan
is one transaction, a job is two rows — and a single connection makes
"database is locked" structurally impossible rather than something to handle.

The cost is binary size: `CGO_ENABLED=0 GOOS=linux go build` now produces a
12 MB static binary, up from about 5 MB. Against a ~450 MB runtime image whose
bulk is the Intel media driver stack, that is not a number worth optimising.

### `status`

The audit surface until the web UI arrives, written for somebody who last
looked six months ago: what is outstanding, how many encodes were declined on
the bitrate floor, what has actually been reclaimed, what is being held back
after a failure, and the recent history. The floor number is the one to watch
— it is the main quality lever, and if it moves a long way after a config
change, that change was a quality change.

## Phase 5 — Web UI ✅ done

`internal/web` serves six pages; `internal/daemon` is the loop they watch.
`html/template` and `embed.FS`, one vendored copy of HTMX for polling, one
stylesheet. No SPA, no node, no build step, and the binary is still one file.

**Definition of done: met.** What is queued, what is running with live
progress, what was left alone and why, the notes, the failures, the history,
and a button that asks for a pass.

### The loop had to move out of main

The UI has to see what the loop is doing while it is doing it, and to ask it
for a pass. Neither is possible against a local variable in a `for` loop in
`main`, so the loop is now `internal/daemon`, holding the live picture behind
a mutex and a trigger channel buffered by one — a second press while a pass is
running is the same request, not a second pass.

It watches through `runner.OnEvent`, the hook Phase 4 left for exactly this,
so `runner` still knows nothing about either the terminal or the web. The live
model is built entirely from those events, which means what the page can show
is precisely what the events carry, and `TestActivityFollowsARunningJob` pins
that.

`web.Engine` is a two-method interface — `Activity()` and `Trigger()` — so
`serve` can hand over nothing at all and get a read-only view of the database
that says so, rather than a page showing an idle daemon that is not there.

### The decisions page is the one that earns the package

The rest is a dashboard. This one is how you audit that the engine is
behaving: a table of every conclusion by `(Action, Video)` — the two are
separate because a file can be left alone as a whole while its video was
specifically declined on the floor — and under it the files in one bucket,
each with the reason string `decide` produced, verbatim. No status code the
page translates back into English, because that is a second place for the
meaning to drift.

### Two things found by building it

**A safety note used to disappear exactly when it mattered.** Notes live on
the decision row, and that row is deleted the moment the file is replaced. So
a note recorded on work that actually happened was the one case the notes view
could not show. `runner.execute` now copies `Plan.Notes` onto the job row
beside the encoder's own notes; `TestASafetyNoteSurvivesTheFileBeingReplaced`
pins it.

**The decision cache is keyed on the profile, and the UI reads the same
numbers `status` does.** `internal/human` exists so that a file `status` calls
"4.2 GiB" is not "4509715660" on the queue page.

### What can be pressed, and what that can do

Two writes: "run a pass now", which asks the loop for the pass it would have
made at the next interval, and "try again", which clears the backoff on one
file. Neither can make this tool do anything to a file the configuration does
not already say — the settle rules, the floor and every safety check are the
same ones a timed pass goes through.

There is no login, as there was none on the stack this replaces. A POST whose
`Origin` is not this host is refused, so a page on another site cannot start
an encode in the operator's browser; that is the whole of the security model,
and it is a LAN service.

## Phase 6 — Docker + GHCR ✅ done

Multi-stage: a `golang:1.27-bookworm` build stage that cross-compiles, and a
`debian:bookworm-slim` runtime stage holding jellyfin-ffmpeg and nothing else.
**394 MB**, against the 400–500 MB expected and ~2 GB for the Tdarr install
this replaces.

**Definition of done: met.** `docker run` it with a config, a media mount and
`--device /dev/dri`, and it encodes. Verified end to end on real files, not
only in tests: a clip generated in the image, planned, encoded and replaced in
place; the daemon running with the web UI answering and its healthcheck green;
`docker stop` shutting the loop down cleanly through SIGTERM.

### jellyfin-ffmpeg brings its own Intel stack

The reason it is the right base is not that it is a good ffmpeg — it is that
its tree holds `lib/dri/iHD_drv_video.so`, libva, libvpl and libmfx-gen. So
there is no non-free apt component to enable, no `intel-media-va-driver` to
keep in step with the ffmpeg using it, and the driver and the encoder are
upgraded by one pin. That pin is a version *and* a SHA256 in the Dockerfile,
because an upgrade of the one component that touches the GPU should be a
commit that says so rather than something a rebuild does quietly.

The package deliberately keeps itself off `PATH` so it cannot collide with a
distro ffmpeg. Nothing else in this image provides one, so it is symlinked
into `/usr/local/bin` and the `-ffmpeg` / `-ffprobe` flags keep their
defaults.

### linux/amd64 only, deliberately

QSV and VAAPI are Intel x86. An arm64 image would imply hardware transcoding
it could not do, so the Dockerfile refuses any other architecture with a
message saying why rather than building something misleading.

### Root, deliberately

Safety rule 11 gives every replacement the original file's owner, and `chown`
to an arbitrary uid needs `CAP_CHOWN`. Running the image with `--user` still
works and is still safe — the encode is verified before the replace either way
— but replaced files then take the container's uid, and the run records a note
saying it could not set the owner. On a share read by Plex, the \*arr stacks
and SMB, that note is the difference between a working library and a support
call.

### What packaging found: `st_dev` is not the question

The work dir was used whenever it was on the same filesystem as the media,
decided by comparing device numbers. In the container that is wrong, and
wrong in the expensive direction.

`rename(2)` fails with `EXDEV` across two **mount points**, not two
filesystems. Two bind mounts of one host disk report an identical `st_dev` and
still refuse a rename between them — which is exactly the shipped layout,
`/temp` and the media as separate `-v` mounts. So the comparison said "same
filesystem, use the work dir", ffmpeg encoded the whole file into `/temp`, and
the replace then failed at its very last step:

```
replacing /media/movies/Demo Film (2021).mkv: rename /temp/.Demo Film …partial.mkv
  → /media/movies/Demo Film (2021).mkv: invalid cross-device link
```

Safe — the original was untouched and the failure was recorded and backed off
— but it would have cost the entire encode, on every file, forever.

The fix is to stop predicting and ask: `encode.CanRenameInto` creates a
zero-byte dotfile in the work dir and renames it into the media directory,
which is precisely the operation being predicted. `run` and `daemon` do it
once at startup, per library root, and drop the work dir for that process when
it fails, with a note saying so. `plan` never does it, because `--dry-run`
writes nothing at all — so `tempPath` keeps the device comparison as its
dry-run prediction, and what a real run relies on is the probe that has
already cleared the work dir before `Prepare` is reached.

The consequence for the operator is in the config comment now: a work dir is
only worth setting when it is inside one of the media mounts. A fast scratch
disk mounted separately is not scratch space, it is a different algorithm.

### First start says what to do rather than crashing

There is nothing safe to do without being told which folders to look at, so
the entrypoint writes `config.example.yaml` into `/config` — refreshed on
every start, so after an upgrade the example beside the real config documents
the schema this binary actually understands — and exits saying to copy it.
`version` and `help` are exempt: they answer without reading anything, and an
unconfigured container is exactly the one somebody runs them against.

It also warns when `/dev/dri` is absent, because a hardware encoder without a
render node fails on every single file, one at a time, hours apart. Not fatal:
the config may name `libx265`, and refusing to start would be this tool
deciding it knows better than the file it was given.

### CI

`ci.yml` on every push and PR: gofmt, `go vet`, `go test -race`, and a build
of the image, which then has to prove the three things it exists to provide —
the binary runs, `ffprobe` is on `PATH`, and `ffmpeg -encoders` lists
`hevc_vaapi` and `hevc_qsv`. ffmpeg is installed in the runner because the
encoder's and runner's tests skip themselves without it, which would quietly
retire the interesting half of the suite.

`release.yml` on a `v*` tag: the tests again — a tag is the one build nobody
gets to re-do — then GHCR, and the bare binary attached to the release with
its SHA256, because the program is one static file that needs nothing but an
ffmpeg on `PATH` and somebody running it outside Docker should not have to
build it.

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

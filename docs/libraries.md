# Library layout and the constraints it imposes

Recorded from the operator's description of the real setup. This drives the
config schema more than anything else does.

## Content types

All four ultimately want the same thing — HEVC in MKV, junk tracks gone — and
differ only in track-selection details.

| Type | Characteristics |
|---|---|
| **Movies** | Subtitles optional. Usually 1 audio stream, at most 2. MKV. |
| **Anime** | Usually soft subs. Audio may be English, Japanese or other. **Language tags are unreliable** — English is sometimes tagged `jpn` and vice versa. |
| **Animated series** | TV shows. Treated like Movies. |
| **TV and other** | Treated like Movies. |

In practice this is **two profiles, not four**: a `standard` one and an `anime`
one that is more conservative about dropping tracks.

### The anime tagging problem

Because language tags cannot be trusted, the existing keep-list
(`eng,und,jpn,kor`) is deliberately broad — it is a blunt instrument
compensating for bad metadata, not a precise preference.

**Rule: when in doubt, keep the stream.** Dropping an audio track you needed is
unrecoverable; keeping one you didn't costs a few MB. The anime profile biases
hard toward keeping, and treats an *untagged* stream as a keep.

In practice the operator's rule is more specific than a keep-list, and
`internal/decide` now implements it directly:

> Sometimes an anime shows up with a Korean audio track and no Japanese one.
> In those cases leave the audio as is. When there are clearly extras beyond
> English and Japanese, that's when I prune.

So pruning on language is gated on the tags looking credible in the first
place. `primary_languages` names what the library would be expected to contain
— `[eng, jpn]` for anime — and if a file has no track in any of them, no track
is dropped for its language at all. A release with only a Korean track is much
more likely mistagged than genuinely unwanted.

Two further rules follow from the same reasoning:

- **The last audio track is never dropped**, whatever the keep-list says. Two
  files in the golden corpus have a single track tagged `chi` and `aze`;
  applying the list literally turns them into silent videos.
- **Forced subtitles outrank the language list.** A forced track is the
  translated-signage track for a film watched in its original audio.

Finally, tracks are *tagged* rather than only filtered: `tag_untagged_as`
writes a language onto untagged streams so Plex's track picker shows something
identifiable instead of "Unknown".

## Folder location is load-bearing — do not reorganise

Some content is deliberately kept **outside** the folders Plex scans. This is a
parental-access measure: rather than rely on Plex profiles and passwords (which
are a friction problem on the Apple TV), certain movies, TV and anime are
physically located outside the scanned library roots so they cannot be
stumbled onto.

This produces hard requirements:

1. **The tool must never move or rename a file across directories.** In-place
   replacement only. A "tidy up your folders" feature would break the
   safeguard, and must never be added.
2. **No Plex integration that scans or announces these paths.** If a Plex
   refresh trigger is ever added it must be per-library and off by default.
3. Isolated content still needs encoding. It is not excluded from the tool's
   job — only from Plex's.

The folder organisation is admittedly ad hoc, with content of the same type
living in several unrelated places. **The tool should treat that as a given and
adapt to it, never the reverse.**

## The abstraction Tdarr lacks

This is the core motivation for the project.

In Tdarr, a "library" bundles together the folder, the plugin stack, and every
plugin's settings. A dozen folders that all want identical treatment therefore
require a dozen libraries, each with its own duplicated stack, kept in sync by
hand through the UI. There is no way to say "these paths, that recipe."

The result is that folders outside the two main Tdarr libraries simply never
got set up — the isolated content and several other roots are unprocessed
today purely because the per-folder setup cost was not worth paying.

**media-compressor splits the two concepts:**

- A **profile** describes the work: encoder, bitrate rules, track selection.
  Defined once.
- A **library** is a name, a profile reference, and a list of roots.

Adding a folder becomes one line of YAML. Changing the recipe for everything
is one edit in one place.

```yaml
libraries:
  - name: movies
    profile: standard
    paths:
      - /mnt/media_video/movies
      - /mnt/media_isolated/movies     # outside Plex, same treatment
```

## Non-goals

The operator's own framing: *"I don't need exhaustive extensibility. I always
do the same flow of encodes no matter what."*

Accordingly, out of scope:

- A plugin system or scripting runtime.
- Arbitrary user-defined filter graphs.
- Distributed worker nodes.
- Per-file manual overrides in the UI.
- Anything that reorganises the filesystem.

# Tdarr stack analysis

Forensics on the Tdarr install this project replaces, done against a copy of
its appdata (`~/Downloads/tdarr`, a temporary directory). Everything needed
from that copy is recorded here or in `testdata/`.

Tdarr version at time of analysis: 2.86.01. Two libraries, both using classic
plugin stacks (not Flows).

## The pipeline

`pluginIDs` from `DB2/SQL/database.db` → `librarysettingsjsondb`. Four of the
five plugins are `-c copy` remuxes; only the last one encodes.

| # | Plugin | Effect |
|---|---|---|
| 0 | `lmg1_Reorder_Streams` | move video to stream index 0 |
| 1 | `vdka_Remove_DataStreams` | `-map 0 -c copy -dn -map_chapters -1` |
| 2 | `00td_action_remux_container` | `-map 0 -c copy` into mkv |
| 3 | `MC93_Migz3CleanAudio` | drop unwanted-language + commentary audio |
| 4 | `MC93_Migz4CleanSubs` *(Anime only)* | same for subtitles |
| 5 | `drdd_standardise_all_in_one` | the only real encode |

Per-library inputs to the encoder plugin:

| Library | Path | qsv | subtitle languages | min target bitrate |
|---|---|---|---|---|
| Media | `/mnt/media` (ignores `anime`, `movies_optimized`) | true | `eng,und,jpn,kor` | 3000 |
| Anime | `/mnt/media/anime` | true | `eng,und,jpn,kor` | 3000 |

Audio language keeps differ: Media uses `eng,und,jpn,kor,fra,fre` with
`tag_language=eng`; Anime uses `eng,und,jpn,kor` with `tag_language=jpn`.

## The encode command

Verbatim from job report `OBgt_POBqd` (720p source):

```
tdarr-ffmpeg -hwaccel vaapi -hwaccel_device /dev/dri/renderD128 -hwaccel_output_format vaapi \
  -i "input.mkv" \
  -map 0 -map -0:d \
  -c:v hevc_vaapi -b:v 3032k -minrate 2122k -maxrate 3941k -bufsize 4549k \
  -c:a copy -c:a:0 ac3 \
  -c:s copy -map -0:s:1 \
  -max_muxing_queue_size 4096 output.mkv
```

Note the plugin input is named `qsv` but the encoder is **`hevc_vaapi`**, not
`hevc_qsv`. It is VAAPI on the Intel iGPU throughout.

## Why the output looks so good

Two mechanisms, neither of them the encoder.

### 1. The bitrate targets are very high

`calculateBitrate()` derives a source bitrate from file size and duration, then
divides by a tier-dependent factor.

**The exact arithmetic**, recovered from the plugin source and verified to
reproduce all 364 fixtures that record numbers:

```js
// getFileDurationInMinutes()
duration = parseFloat(ffProbeData.format.duration) * 0.0166667   // if > 0
         : meta.Duration * 0.0166667                             // else
         : ffProbeData.streams[0].duration * 0.0166667           // else

original = ~~(file_size_MiB / (duration * 0.0075))
target   = ~~(original / divisor)
min      = ~~(target * 0.7)
max      = ~~(target * 1.3)
bufsize  = original
```

Three details matter for reproducing it, all ported in `internal/decide`:

- `file_size` is **binary** megabytes (bytes / 1048576).
- `~~` truncates toward zero at every step, so the errors do not average out.
- The seconds-to-minutes constant is **`0.0166667`, not `1/60`**. It is very
  slightly larger, so every computed bitrate comes out very slightly lower.
  The difference is under a tenth of a percent, but at a truncation boundary it
  changes the integer — enough to move roughly one fixture in thirty. This is
  `decide.Quirks.MinuteFactor`, and it is the reason the corpus needs a compat
  profile at all.

The tier table itself:

| Source bitrate | Divisor |
|---|---|
| ≥ 10000 kbps | 2.0 |
| 6000–9999 | 1.75 |
| 3000–5999 | 1.5 |
| < 3000 | 1.0 (no reduction) |

Then `target ±30%` becomes minrate/maxrate, and `bufsize` is set to the full
original bitrate.

Across 177 distinct decisions found in the job reports: median source 5156
kbps, median target **3436 kbps**, and the overwhelmingly common ratio is
**1.50x**.

**It is more generous than that looks.** The "original bitrate" is computed
from *total file size* — video plus audio plus subtitles — and that whole
budget is then handed to the *video stream alone* while audio is copied on top.
On a sample file: 4772 kbps total, of which video was 4229 kbps. So a nominal
1.5x container reduction is about **1.35x on the actual video**.

HEVC is roughly 40% more efficient than H.264 at equal quality. Encoding
H.264 → HEVC at ~74% of the original video bitrate spends about double what
transparency needs. That is the whole trick.

### 2. It refuses the hard jobs

`minimum_target_bitrate = 3000` skips any file whose target would land below
3 Mbps. Across all 16,013 job reports:

| Decision | Count |
|---|---|
| Skipped — target bitrate below floor | **7,880** |
| Already HEVC + MKV, untouched | 8,145 |
| Actually transcoded | **191** |
| Remux only | 0 |

Every file needing aggressive compression was passed over. There are no bad
Tdarr encodes because Tdarr declines to make them. This survivorship filter
does more for perceived quality than the encoder does.

**Implication for this project:** matching Tdarr's quality is a matter of
matching its *bitrate math and its skip rule*, not of finding a magic encoder.
`hevc_vaapi` is in fact mediocre per-bit; x265 or `hevc_qsv` with ICQ beat it
at equal bitrate. It wins here by being handed twice the bits.

**And the floor was doing more than its job title suggests.** It is the reason
none of the 28 AV1 files was downgraded to HEVC, and the reason none of the 12
files with PNG posters had the poster encoded as video. Both of those were
latent bugs that the floor happened to mask. Anyone tempted to lower it — and
there is a real case for doing so, since it currently excludes thousands of
files — should note that it has been silently covering for other things, all of
which are now fixed and tested independently.

## Bugs found, and what was done about them

These came out of making the golden corpus pass. Each is now covered by a test
in `internal/decide`; see docs/plan.md for the fixes.

### PNG cover art was treated as a video stream

`buildVideoConfiguration` loops over every video stream and treats each as an
encode candidate. It recognises `mjpeg` as artwork and skips it — but nothing
else. A file with an attached PNG poster therefore has a "video stream" that is
not HEVC, and falls straight into the transcode branch.

Twelve corpus fixtures are exactly this: HEVC video, an MJPEG thumbnail that
was correctly skipped, and a 1920x1080 PNG poster that was not. All twelve were
saved from having their poster re-encoded as an hour of video only by the
bitrate floor. This is the clearest illustration of the point below — the floor
was doing work nobody designed it to do.

### AV1 was an encode candidate

The branch is `if (stream.codec_name !== "hevc")`, so AV1 qualifies. AV1 is
*more* efficient than HEVC, making that a straight downgrade in both quality
and size. All 28 AV1 fixtures fell below the bitrate floor, so it never fired —
again by luck rather than design.

### A language keep-list can leave a file silent

Not a bug in this plugin (audio language filtering was `MC93_Migz3CleanAudio`),
but a bug in the approach. Two corpus files have exactly one audio track,
tagged `chi` and `aze`. A keep-list of `eng,und,jpn,kor,fra,fre` applied
literally drops it and produces a video with no audio — unwatchable, and
unrecoverable once the source is deleted.

### Cosmetic quirks, reproduced for exactness

- `-map -v:N` instead of `-map -0:v:N` for artwork. ffmpeg accepts both.
- A stream dropped for two reasons at once (an unwanted language that is *also*
  titled "Commentary") emits `-map -0:s:N` twice. ffmpeg ignores the repeat.
  44 of the 1,877 recorded commands contain it.
- `hasMultiChannelAudio` is assigned rather than OR'd inside the audio loop, so
  only the *last* audio stream decides it. Affects log wording only.

## Gotchas found

- **`Plugins/Local/` is dead code.** Both libraries reference
  `source: "Community"`, so `Plugins/Community/Tdarr_Plugin_drdd_standardise_all_in_one.js`
  is what actually runs. The `Local/` copy is an older fork still using the
  legacy `*_cuvid` decoder chain. Edits there never took effect.
- The `Local/` fork additionally has a broken nested `else if` in its NVENC
  branch: the `mjpeg`/`mpeg1`/`mpeg2`/… cases are nested inside the `h264`
  branch and are unreachable.
- `getFileDurationInMinutes()` uses `typeof x != undefined` — comparing a
  string to `undefined`, which is always true. The `meta.Duration` branch is
  therefore always taken. Harmless in practice, but do not port it.
- Bitrate basis is the container, not the video stream (see above). This is a
  bug, but it is the bug that produces the quality being replicated, so
  `source_bitrate_basis: container` is the compatibility default.

## Scale reality check

5,817 lifetime transcodes on Media (438 GB saved), 1,066 on Anime (12 GB).
Both libraries are now in steady state — 191 encodes across the entire
retained job history. The workload does not justify a job queue with a
distributed node protocol.

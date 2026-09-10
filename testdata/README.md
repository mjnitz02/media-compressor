# Golden decision corpus

`tdarr_decisions.jsonl.gz` — 1,881 deduplicated decision fixtures extracted
from the real Tdarr install this project replaces.

## Provenance

Produced by `tools/extract_tdarr_corpus.py` from a copy of the Tdarr appdata
at `~/Downloads/tdarr` (Tdarr 2.86.01, 16,202 transcode job reports). **That
directory is temporary and will be deleted** — this corpus is the durable
record of how the old stack behaved.

## Shape

One JSON object per line:

```jsonc
{
  "report": "OBgt_POBqd()2.77.01()transcode()o4D15Djk4()...txt",
  "kind": "transcode",              // transcode | skip_bitrate_floor | already_done | remux | no_op
  "source": {
    "container": "mkv",
    "video_codec_name": "h264",
    "file_size": 821.9,             // MB, as Tdarr computed it
    "duration": 1376,               // seconds
    "meta_duration": "00:22:56...",
    "ffProbeData": { "streams": [...], "format": {...} }
  },
  "expected": {
    "infoLog": "☒ Transcoding to HEVC using VAAPI\n• Target Bitrate: 3184\n...",
    "cli": "tdarr-ffmpeg -hwaccel vaapi ... -c:v hevc_vaapi -b:v 3184k ...",
    "acted": true                   // was this the plugin that produced `cli`?
  },
  "outcome": "success"
}
```

`expected.cli` is populated only when the encoder plugin was the one that acted
in that cycle (`acted: true`); otherwise another plugin in the stack produced
the command and it is not attributable.

## Composition

| kind | count | meaning |
|---|---|---|
| `already_done` | 1,517 | already HEVC in MKV, no action |
| `skip_bitrate_floor` | 203 | target below `min_target_bitrate`, encode declined |
| `transcode` | 161 | real encode (158 with the exact ffmpeg command) |

Fixtures are deduplicated on `(kind, file_size, duration, infoLog)` so the set
stays representative rather than dominated by repeated passes over the same
files.

## How it is used

Phase 1's `internal/decide` must reproduce `expected` for every fixture. See
[docs/plan.md](../docs/plan.md).

The `skip_bitrate_floor` cases matter as much as the transcodes — declining to
encode is the behaviour most responsible for the old stack's output quality.

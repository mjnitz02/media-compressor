#!/usr/bin/env python3
"""
Extract a golden-decision corpus from a Tdarr JobReports directory.

Each Tdarr transcode report contains, in plain text:
  * `fileVersionOriginalLogJSONString:{...}` - the source file's full ffProbeData
    as Tdarr saw it before any plugin ran.
  * For each plugin in the stack, a `Plugin: <id>:` marker followed by that
    plugin's response JSON (`processFile`, `preset`, `infoLog`, ...).
  * The assembled `lastCliCommand` actually handed to ffmpeg.

Pairing the source ffProbeData with the drdd plugin's response gives us a
(probe input -> decision output) fixture. These are the ground truth for
media-compressor's own decision engine.

The Tdarr appdata this reads is temporary; the output corpus is not.
"""
import argparse
import glob
import gzip
import json
import os
import sys

MARK_ORIGINAL = "fileVersionOriginalLogJSONString:"
MARK_CYCLE = "pluginCycleLogJSONString:"
MARK_VERSION = "fileVersionLogJSONString:"
PLUGIN_SECTION = "Pre-processing - "
TARGET_PLUGIN = "Tdarr_Plugin_drdd_standardise_all_in_one"

decoder = json.JSONDecoder()


def decode_after(text, marker):
    """Decode the JSON object following `marker`. Returns dict or None."""
    i = text.find(marker)
    if i == -1:
        return None
    b = text.find("{", i)
    if b == -1:
        return None
    try:
        obj, _ = decoder.raw_decode(text, b)
    except ValueError:
        return None
    return obj if isinstance(obj, dict) else None


def slim_source(source):
    """Keep only what a decision engine could legitimately look at."""
    probe = source.get("ffProbeData") or {}
    streams = []
    for s in probe.get("streams") or []:
        streams.append({
            k: s[k] for k in (
                "index", "codec_name", "codec_type", "profile",
                "width", "height", "pix_fmt", "bit_rate", "channels",
                "channel_layout", "sample_rate", "r_frame_rate", "avg_frame_rate",
                "duration", "disposition", "tags",
            ) if k in s
        })
    fmt = probe.get("format") or {}
    return {
        "container": source.get("container"),
        "video_codec_name": source.get("video_codec_name"),
        "video_resolution": source.get("video_resolution"),
        "file_size": source.get("file_size"),
        "duration": source.get("duration"),
        "meta_duration": (source.get("meta") or {}).get("Duration"),
        "ffProbeData": {
            "streams": streams,
            "format": {
                k: fmt[k] for k in
                ("duration", "size", "bit_rate", "format_name", "nb_streams")
                if k in fmt
            },
        },
    }


def plugin_section(worker_log, plugin_id):
    """Pull one plugin's slice out of the concatenated worker log."""
    if not worker_log:
        return None
    parts = worker_log.split(PLUGIN_SECTION)
    for part in parts[1:]:
        head, _, body = part.partition("\n")
        if head.strip() == plugin_id:
            return body.strip()
    return None


def parse_report(path):
    try:
        text = open(path, errors="replace").read()
    except OSError:
        return None
    if MARK_ORIGINAL not in text or TARGET_PLUGIN not in text:
        return None

    original = decode_after(text, MARK_ORIGINAL)
    if not original:
        return None
    source = (original.get("sourceFile") or {})
    if not source.get("ffProbeData"):
        return None

    cycle = decode_after(text, MARK_CYCLE) or {}
    log = plugin_section(cycle.get("workerLog"), TARGET_PLUGIN)
    if log is None:
        return None

    version = decode_after(text, MARK_VERSION) or {}
    acting = version.get("lastPluginId") or ""
    cli = cycle.get("lastCliCommand") or ""

    return {
        "report": os.path.basename(path),
        "source": slim_source(source),
        "expected": {
            "infoLog": log,
            # The CLI is only attributable to this plugin when it was the one
            # that acted in this cycle; otherwise another plugin produced it.
            "cli": cli if acting == TARGET_PLUGIN else "",
            "acted": acting == TARGET_PLUGIN,
        },
        "outcome": cycle.get("outcome"),
    }


def classify(fixture):
    log = fixture["expected"].get("infoLog") or ""
    if "Skipping video encoding as target bitrate" in log:
        return "skip_bitrate_floor"
    if "Transcoding to HEVC using" in log:
        return "transcode"
    if "File is in HEVC codec but not MKV" in log:
        return "remux"
    if "File is in HEVC codec and in MKV" in log:
        return "already_done"
    if "No need to process file" in log or "No video processing necessary" in log:
        return "no_op"
    return "other"


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("jobreports", help="path to Tdarr DB2/JobReports")
    ap.add_argument("-o", "--out", required=True, help="output .jsonl.gz")
    args = ap.parse_args()

    paths = sorted(glob.glob(os.path.join(args.jobreports, "*", "*transcode*.txt")))
    print(f"scanning {len(paths)} transcode reports", file=sys.stderr)

    counts = {}
    seen = set()
    written = 0
    with gzip.open(args.out, "wt") as out:
        for n, p in enumerate(paths):
            if n and n % 2000 == 0:
                print(f"  ...{n}", file=sys.stderr)
            fx = parse_report(p)
            if not fx:
                continue
            kind = classify(fx)
            # Dedupe identical decisions so the corpus stays representative
            # rather than dominated by repeat passes over the same file.
            key = (
                kind,
                fx["source"].get("file_size"),
                fx["source"].get("duration"),
                fx["expected"].get("infoLog"),
            )
            if key in seen:
                continue
            seen.add(key)
            fx["kind"] = kind
            counts[kind] = counts.get(kind, 0) + 1
            out.write(json.dumps(fx, separators=(",", ":")) + "\n")
            written += 1

    print(f"\nwrote {written} fixtures to {args.out}", file=sys.stderr)
    for k, v in sorted(counts.items(), key=lambda kv: -kv[1]):
        print(f"  {k:22s} {v}", file=sys.stderr)


if __name__ == "__main__":
    main()

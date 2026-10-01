# UI redesign: making 40,000 files legible

The current UI works at the scale it was built and tested against. It does not
work at the scale the operator actually has: **5–8 libraries, 300 to 10,000
files each, upwards of 40,000 files in total.**

This document records what breaks, why, and the shape that replaces it. It is
a design record, not a build plan — the reasoning is the part worth keeping,
because most of these mistakes are easy to make a second time.

## The governing problem: partitioning by "why" does not scale

Five of the six current tabs — Outstanding, Decisions, Notes, Held back,
History — are the same noun, a file, sliced by decision state. That partition
has one fatal property at 40,000 files: **every bucket is either enormous or
empty.** There is no library size at which a flat "Decisions" page is useful.

The axis that does partition well is missing entirely. `files` has no library
attribution, so the operator's actual question — *how is the anime library
doing?* — cannot be asked. Libraries appear exactly once, as a static config
dump on the overview.

This is the one thing the stack being replaced got right. Tdarr's per-library
view, with statistics at the top and a filterable table underneath, is a good
idea buried in a bad product. Take the idea.

## Three levels

Nav becomes **`Now · Libraries · Failures · History`** plus a search box. No
badges except a single indicator on Now when something is genuinely stuck.

### L1 — `/` "Now"

Answers only *what is happening, and does anything need me*.

- What is running, with progress. This is the best part of the current UI;
  it survives nearly unchanged. Give it a fixed minimum height so the page
  stops resizing under the cursor every three seconds.
- One stacked bar for the whole estate: done · outstanding · left alone ·
  declined on the floor · not yet decided. The ~35,000 no-op files become a
  grey segment instead of 35,000 rows.
- Per-library rows, each a small version of that bar, plus last pass and
  space reclaimed.
- A needs-you line, **absent when zero**, carrying only genuinely actionable
  things (see the failure and note taxonomies below).

Removed from this page: the config dump, the decision-groups table, the
recent-passes table, the recent-jobs table. Four tables on a landing page is
a table of tables.

### L2 — `/library/{name}`

That library's bar, its profile and paths, last pass, space reclaimed. Then
**one** table, with a segmented filter:

```
[ needs work 412 ] [ left alone 8,901 ] [ declined 7,880 ] [ failed 3 ] [ notes 12 ] [ all ]
```

**The default filter is "needs work".** That default is the whole scale fix:
the 9,000-row set is one click away and is never rendered by accident.

One row per file. Basename only, full path on hover. No detail sub-rows — the
current jobs table emits extra `<tr>`s per error and per note, so one job can
occupy four lines and a "250 row" page is not 250 lines.

### L3 — `/file?path=…`

Everything known about one path, in one place: the current decision with its
verbatim reason, notes with their levels, settle state, failure history, every
job that has touched it with its ffmpeg tail, and the retry button.

This is the biggest gap in the current UI — there is no file view at all, so
everything about one file is scattered across four pages and joinable only by
eye. It is also where all the detail currently crammed into table cells
belongs, which is what lets L2 stay one line per row.

## Failures: four categories, and only two deserve a retry

Every failure currently lands in the same free-text `last_error` and gets the
same treatment: red, and a uniform backoff of 1h → 6h → 24h → give up. The
real error sites split cleanly, and the categories want different handling.

| | Examples | Actionable? | Retry helps? |
|---|---|---|---|
| **A. The file is broken** | `ffmpeg failed: …`, probe fails during prepare | **Yes — the primary signal.** The source is likely damaged; look at it, re-acquire it | No, but cheap to confirm once |
| **B. The output was rejected** | Verification: does not ffprobe cleanly, duration drift >1s, missing streams, implausible size | **Yes, loudly.** Encoder settings are wrong or the source is pathological. Original untouched, temp kept | Rarely |
| **C. The environment is wrong** | Work dir unusable, ffmpeg will not start, rename or remove denied | Yes — but **one cause, N files** | **Yes** |
| **D. We deliberately refused** | Symlink, output already exists, source changed mid-encode | It is a decision, not a fault | **Never** |

Two consequences:

**D is not a failure and must not be styled as one.** A symlink is still a
symlink in an hour. "Output already exists" is still true tomorrow. These burn
three retry cycles and a full day to arrive at the answer they had
immediately. They belong in a permanent, quiet "declined — needs a decision"
state with no backoff at all.

**C must collapse to one line.** If the render device is missing, every encode
fails identically. Four thousand rows is a strictly worse presentation of that
fact than one line reading *"ffmpeg could not start — 4,000 files — check the
device"*. Deduplicate by cause, then show the affected count.

That leaves **A and B as the Failures page**, which is the signal the operator
actually wants: *this file failed the ffmpeg encode and may be damaged.* At
40,000 files that page should be short. If it is not, something systemic is
wrong, and the C-style rollup above it will say what.

### Reason codes

This requires a `reason_code` recorded where the error is raised —
`ffmpeg.decode`, `verify.duration`, `verify.streams`, `refused.symlink`,
`env.device` — and **not** regex-matched out of the message afterwards. The
code decides routing, styling and retry policy. The verbatim text stays beside
it and remains the explanation; deriving one from the other in the UI would be
a second place for the meaning to drift.

## Notes: three levels, and most of them are Info

There are exactly four note-producing sites in the codebase. The current UI
treats all four identically, as a yellow badge in the nav.

| Note | Volume | Level |
|---|---|---|
| "kept every audio track: no track is in `eng`, so the language tags on this file are not trustworthy enough to prune on" | **High** — near-constant on the anime library | **info** |
| "kept audio track N (…) even though it matches no keep rule: dropping it would leave the file with no audio" | 2 in 1,881 | **notable** |
| "could not set owner N:N on the replacement" | All-or-nothing | **warning** |
| "the original had N hard links, so the old data stays on disk" | Occasional | **info** |

The first is the highest-volume note in the system, and it is not a warning in
any useful sense. It records that **standard processing happened, and why it
came out conservative** — the tag-credibility rule working exactly as designed.
On the anime library it fires on a large fraction of files. Surfacing it as a
yellow badge is the single biggest reason the Notes tab means nothing.

The ownership warning has the same shape as failure category C: a wrong
`PUID`/`PGID` makes it fire on *every* job. It must deduplicate by cause too —
"ownership could not be set on 4,102 replacements" is one configuration
problem, not 4,102 warnings.

So:

- **info** — never surfaces proactively. File page, and a filter chip.
- **notable** — a count on the library page. Worth a periodic look, not an alarm.
- **warning** — a deduplicated line on Now.

Mechanically this is cheap. Notes are newline-joined strings, so `code\ttext`
per line carries the level with no migration and no new table. Emitting a code
from `decide` is pure, so the no-I/O rule holds.

## What 40,000 files actually breaks

Not rendering. The problem is that `base()` runs on every page load and
computes everything, for a nav that needs at most two numbers:

- `notes <> ''` on `files` *and* `jobs` — `notes` is unindexed text, so two
  full scans.
- `DecisionGroups` — `GROUP BY action, video`, a full scan, rendered on two
  different pages.
- `Counts` — eight aggregates, most of them unindexed.

That is roughly ten scans of a 40,000-row table per page view, **on the single
connection the scanning pass is writing through** — the same constraint that
already keeps the activity poll away from the database. During a first pass
over 40,000 files, that is real contention.

In order of payoff:

1. **`base()` computes only what the nav shows.** With the badges gone, that
   is one number or none.
2. **Index `files(action, video)`.** Serves the decision rollup and every
   filter chip.
3. **One conditional aggregate per library** instead of one query per chip:

   ```sql
   SELECT SUM(action IN ('encode','remux') AND failures = 0) AS needs_work,
          SUM(action = 'none')                               AS left_alone,
          SUM(video  = 'skipped_floor')                      AS declined,
          SUM(failures > 0)                                  AS failed,
          COUNT(*)                                           AS total
   FROM files WHERE path >= ? AND path < ?
   ```

   One range scan gets a library's entire chip row. Eight libraries is eight
   scans for the whole Libraries page.

Keep the existing split where the fast activity poll touches no database at
all, and put the aggregates on a slow refresh or a manual one.

### Library attribution: derive it, do not store it

A file belongs to whichever library it lives underneath. `config.LibraryFor`
already exists and overlapping library paths are already a validation error,
so a key range on the primary key is exact and index-backed:

```sql
WHERE path >= '/mnt/media_video/anime/' AND path < '/mnt/media_video/anime0'
```

(`0` is the character after `/`.) Prefer this to `LIKE` — it rides the primary
key directly and needs no escaping, though note `Sweep` already establishes
the escaped-`LIKE`-prefix pattern if consistency matters more.

The alternative is a `library` column on `files`, populated at scan time. It
buys a single `GROUP BY library`, and costs a migration plus a value that goes
stale the moment a path is edited in YAML. **The library is configuration.**
Caching it in SQLite is exactly the mixing that the config-in-YAML,
state-in-SQLite rule exists to prevent. Derive it.

## Pagination and filtering

**Keyset, not offset.** `WHERE <filter> AND path > ? ORDER BY path LIMIT 101`
— fetch one extra row to learn whether a next page exists without a second
`COUNT`. The cursor is the last path. This rides the primary key, does no
offset scan, and stays stable while a pass inserts rows underneath it. In
HTMX the "show more" control is a sentinel row that `hx-swap="outerHTML"`s
itself into the next batch plus a fresh sentinel.

This replaces the 250-row wall, and with it every inconsistent apology for it:
today `queue` says "Showing 250 of 4213", `skipped` says "Showing the first
250" — and computes that from `len(files) == listLimit`, which lies when a
group holds exactly 250 — while `history`, `notes` and `blocked` simply stop
and say nothing.

**Two sorts, not arbitrary columns.** Path (default) and size descending;
*what are my biggest outstanding files* is a real question, and a two-column
`(size, path)` cursor handles it. Arbitrary sortable headers need a cursor per
column for very little gain.

**All state in the URL**: `?state=needs-work&sort=size&q=inception`.
Bookmarkable, the back button works, and HTMX gets `hx-push-url` for free.

**Search is `path LIKE '%…%'`** — a full scan, tens of milliseconds on 40,000
rows. Take it. FTS5 is a new dependency and a second index to keep in sync,
for a query that is already fast enough.

Chip counts are exact, from the aggregate above. The row list is paged. That
combination is what lets the UI say "8,901 left alone" without ever rendering
8,901 rows.

## Grouped reasons, and the one histogram worth building

Reason strings embed numbers — `"target bitrate 2410 kbps below floor 3000
kbps"` — so `GROUP BY reason` does not collapse anything. Group by
`(action, video)` as the current rollup already does, and show one exemplar
reason per group.

For the floor-declined bucket specifically, show **a histogram of how far
under the floor each file landed**, not a list. That answers the question the
operator would actually ask — *would dropping the floor to 2500 unlock much?*
— which no list of 7,880 rows will ever answer. Given that the floor is the
single most important quality control in the system, this is the highest-value
screen in the redesign.

## What gets deleted

Outstanding, Decisions, Notes and Held back stop being pages and become filter
chips inside a library. The six page-level explanatory paragraphs become a
disclosure per page, with the words preserved in the README — they are
excellent documentation and they are not interface, and at present they sit
above the fold on every page on every visit forever.

Net effect: the nav goes from six items to four, no page renders more than one
table, and the largest library's default view is its few hundred needs-work
files, paged a hundred at a time.

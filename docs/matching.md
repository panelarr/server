# Works and source matching

Panelarr keeps **one work per actual work**, not one per site. A work is created from an entry of the metadata catalogue the first time anyone adds it; everyone who adds the same entry shares that work. Each user's library is the list of works they follow.

## How a work finds its series on the sources

Sites name the same series differently (an English title, a romanized one, a translation), and many only know one title. So when a work is added, the server searches every source that can search, one task per source (so each source's rate limits apply):

1. It searches with the work's main title, then with its other titles: first those in the source's language, then romanized ones, then English, then the rest (at most 5 searches per source).
2. It stops at the first search that turns up results whose title is close enough (similarity at least 0.6) to any of the work's titles.
3. For the best few results it fetches the full details: the authors and artists, and how many chapters there are.
4. A result is attached automatically when its title matches one of the work's titles exactly (after normalizing case, width, accents and punctuation) **and** it shares an author or artist with the work, and it is the only such result. Everything else is kept as a **candidate** with its evidence (the title that matched, the search, the similarity, whether the people match, the chapter count).

Each source then shows one of: matched, candidates, no match, or failed (with the reason, e.g. the site being down; the task retries).

## Fixing matches

Users with the `may_manage_matches` grant (and admins) can:

- attach a candidate, or another series by source and key, or by pasting its URL (a plugin that recognises the URL resolves it);
- detach a series: it is remembered as rejected and never attached automatically again;
- set a series' priority (used when the same chapter is on several sources);
- run matching again (optionally including sources that already have a series; attached series stay).

## Adding from a source

A series found while browsing a source (an admin view) can be added directly. The server searches the catalogue with the series' titles; when the entry is clear (an exact title and a shared author, and only one such entry) the work is added with the series attached. Otherwise the likely entries are offered to pick from.

## Who sees what

A work is shown to a user when its content rating is within their ceiling and it either has no series attached yet or has one on a source they may use. Removing a work from a library never deletes files.

## Tuning

The instance setting `matching` (JSON) overrides the defaults: `max_queries` (5), `candidate_score` (0.6), `max_candidates` (5), `details` (3).

## Chapters

Every attached series' chapters are kept exactly as its source lists them (the per-source view on a work's page). Across sources, chapters with the same number become **one chapter**. Numbers come from the source, or from the name when the source gives none: "Chapter 12.5", "Ch. 3", "Ep. 7", "#7", "第12话", a bare "12", ranges such as "10-11" (by their first number). The volume comes from the source, or from the name ("Vol. 2", "Volume 2", "v2", "第2卷"), and a chapter takes the one most of its copies give. Names starting with "Extra", "Special", "Side Story", "Bonus", "Notice", "Hiatus" and the like are extras: they stand apart from the numbered run even when the source numbers them. Chapters without a number (extras included) stay per source and are listed after the numbered ones. Each chapter also gets a **display title**, its own name without the number, volume, release tags at either end (official, fan translation, scanlation, TL, MTL, raw, HQ, LQ, …), bracketed tags and a trailing group name in parentheses ("Vol. 1 Volume 1 Chapter 1: Double Dungeon" → "Double Dungeon"; "Ch. 200 OFFICIAL TRANSLATION" → none, shown as "Chapter 200"); downloads use it as the ComicInfo title. After an update that reads chapter names better, the `chapters.reunify` job (run by hand) rebuilds every work's chapters from what's stored, without asking any source. So a chapter missing on one source is filled from another.

Each chapter shows one chosen copy and why:

1. `pinned`: a user with `may_manage_matches` chose it (until it disappears from its source, or while its source is unusable and another copy isn't);
2. `healthier_source`, first for usability: a copy on a usable source beats one on a source that is turned off, no longer listed by its plugin (absent) or whose plugin is turned off;
3. `language`: the instance setting `chapters.languages` (a JSON list such as `["en", "de"]`) prefers copies in those languages, in order;
4. `healthier_source`: a copy on a working source beats one on a failing source (a failing source still serves a chapter only it has);
5. `priority`: the series' priority for the work;
6. `newest_upload`, then `first_listed`.

`only_source` means just one source has it. A chapter no source lists any more stays (progress and files may point at it) but shows as unavailable.

### When a source goes away

A source can stop being usable: an admin turns it or its plugin off, or its plugin stops listing it (an extension uninstalled, its language turned off). Nothing of the work is deleted: its series stay attached, their chapters, read progress and downloaded files stay, and the work's page keeps the chapters. As soon as the server notices (the admin's switch, or the plugin's next sync), every work with a series on that source chooses its copies again, so reading and downloads move to other sources that have the chapters. Matching, update checks (skipped, not failed) and a work's source search leave the source alone meanwhile. When it is usable again, the copies are chosen again the same way and its copies come back.

### Update checks

The `updates.check` job (every 6 hours) checks every attached series of followed works for chapters, spread over half an hour and within each source's limits; a work's page can ask for a check now. A newly attached series is checked right away. Chapters found after a user last opened a work show as **new** for them until they mark it seen. The **updates feed** (`GET /api/v1/me/updates`) lists, newest first, the chapters that *appeared* in a user's library: found by a check of a series after its first listing, and after the user added the work. When it appeared is when the check found it, not the source's upload date. What a series lists the first time it's checked (a newly added work, or a newly attached source) isn't news, and neither is a chapter made again when the unified list is rebuilt: a variant is announced once, by the first rebuild after the check that found it. Each entry has the chapter's read and download state. `GET /api/v1/me/updates/status` gives the latest check of the user's series, how many checks are running or due and how many series failed their last check; `POST /api/v1/me/updates/refresh` queues a check of every series in the user's library (bringing scheduled ones forward), and the status shows when they're done. Works the user may no longer see are left out of all three. The `works.refresh` job (daily) refreshes works' titles, covers and statuses from the catalogue.

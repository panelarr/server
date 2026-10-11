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

A series found while browsing a source can be added directly. The server searches the catalogue with the series' titles; when the entry is clear (an exact title and a shared author, and only one such entry) the work is added with the series attached. Otherwise the likely entries are offered to pick from.

## Who sees what

A work is shown to a user when its content rating is within their ceiling and it either has no series attached yet or has one on a source they may use. Removing a work from a library never deletes files.

## Tuning

The instance setting `matching` (JSON) overrides the defaults: `max_queries` (5), `candidate_score` (0.6), `max_candidates` (5), `details` (3).

## Chapters

Every attached series' chapters are kept exactly as its source lists them (the per-source view on a work's page). Across sources, chapters with the same number become **one chapter** (numbers are read from the source, or from the name such as "Chapter 12.5" when the source gives none); the volume is the one most sources give. Chapters without a number stay per source and are listed after the numbered ones. So a chapter missing on one source is filled from another.

Each chapter shows one chosen copy and why:

1. `pinned`: a user with `may_manage_matches` chose it (until it disappears from its source);
2. `language`: the instance setting `chapters.languages` (a JSON list such as `["en", "de"]`) prefers copies in those languages, in order;
3. `healthier_source`: a copy on a working source beats one on a failing source (a failing source still serves a chapter only it has);
4. `priority`: the series' priority for the work;
5. `newest_upload`, then `first_listed`.

`only_source` means just one source has it. A chapter no source lists any more stays (progress and files may point at it) but shows as unavailable.

### Update checks

The `updates.check` job (every 6 hours) checks every attached series of followed works for chapters, spread over half an hour and within each source's limits; a work's page can ask for a check now. A newly attached series is checked right away. Chapters found after a user last opened a work show as **new** for them until they mark it seen. The `works.refresh` job (daily) refreshes works' titles, covers and statuses from the catalogue.

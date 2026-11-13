// SPDX-License-Identifier: GPL-3.0-or-later

package library

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	metav1 "github.com/panelarr/meta/gen/panelarr/meta/v1"
	metadatav1 "github.com/panelarr/meta/gen/panelarr/metadata/v1"
	sourcev1 "github.com/panelarr/source-api/gen/panelarr/source/v1"
	"github.com/panelarr/source-api/sdk/plugin"

	"github.com/panelarr/server/internal/plugins"
	"github.com/panelarr/server/internal/queue"
	"github.com/panelarr/server/internal/store/db"
	"github.com/panelarr/server/internal/textmatch"
)

// Task kinds.
const (
	KindMatchWork   = "match.work"
	KindMatchSource = "match.source"
)

// Series states and origins.
const (
	StateAttached  = "attached"
	StateCandidate = "candidate"
	StateRejected  = "rejected"

	OriginAuto   = "auto"
	OriginManual = "manual"
	OriginURL    = "url"
	// OriginImport: attached by an import of another app's library.
	OriginImport = "import"
)

// Match statuses per work and source.
const (
	MatchPending    = "pending"
	MatchMatched    = "matched"
	MatchCandidates = "candidates"
	MatchNone       = "no_match"
	MatchFailed     = "failed"
)

// MatchSettings tune matching. They are an instance setting ("matching"),
// with these defaults.
type MatchSettings struct {
	// MaxQueries caps the searches per source (main title, then
	// alternatives).
	MaxQueries int `json:"max_queries"`
	// CandidateScore is the title similarity a result needs to count as a
	// candidate.
	CandidateScore float64 `json:"candidate_score"`
	// MaxCandidates kept per source.
	MaxCandidates int `json:"max_candidates"`
	// Details is how many top results get their full details (people,
	// chapter count) fetched.
	Details int `json:"details"`
}

// DefaultMatchSettings are used unless the instance setting says otherwise.
var DefaultMatchSettings = MatchSettings{MaxQueries: 5, CandidateScore: 0.6, MaxCandidates: 5, Details: 3}

const settingMatching = "matching"

func (s *Service) matchSettings(ctx context.Context) MatchSettings {
	m := DefaultMatchSettings
	raw, err := s.st.GetSetting(ctx, settingMatching)
	if err == nil {
		_ = json.Unmarshal(raw, &m)
	} else if !errors.Is(err, pgx.ErrNoRows) && ctx.Err() == nil {
		s.log.WarnContext(ctx, "read setting", "key", settingMatching, "err", err)
	}
	return m
}

type matchWorkPayload struct {
	WorkID uuid.UUID `json:"work_id"`
	// Force searches sources that have an attached series too.
	Force bool `json:"force,omitempty"`
}

type matchSourcePayload struct {
	WorkID   uuid.UUID `json:"work_id"`
	SourceID string    `json:"source_id"`
}

// Rematch queues matching of a work against every usable source.
func (s *Service) Rematch(ctx context.Context, workID, by uuid.UUID, force bool) (db.Task, error) {
	if _, err := s.st.GetWork(ctx, workID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return db.Task{}, ErrNotFound
		}
		return db.Task{}, err
	}
	t, _, err := s.queue.Enqueue(ctx, queue.NewTask{
		Kind: KindMatchWork, Payload: matchWorkPayload{WorkID: workID, Force: force},
		DedupeKey: "match:" + workID.String(), CreatedBy: by, Priority: 10,
	})
	return t, err
}

// handleMatchWork fans a work's matching out into one task per source, so
// each source's limits apply.
func (s *Service) handleMatchWork(ctx context.Context, t *queue.Task) (any, error) {
	var p matchWorkPayload
	if err := t.Decode(&p); err != nil {
		return nil, queue.Permanent(err)
	}
	srcs, err := s.host.Sources(ctx, adminViewer.plugins(), false)
	if err != nil {
		return nil, err
	}
	attached, err := s.attachedSources(ctx, []uuid.UUID{p.WorkID})
	if err != nil {
		return nil, err
	}
	queued := 0
	for _, src := range srcs {
		if !slices.Contains(src.Capabilities, "search") {
			continue
		}
		if !p.Force && slices.Contains(attached[p.WorkID], src.ID) {
			continue
		}
		if err := s.st.UpsertWorkSourceMatch(ctx, db.UpsertWorkSourceMatchParams{
			WorkID: p.WorkID, SourceID: src.ID, Status: MatchPending, Queries: []string{},
		}); err != nil {
			return nil, err
		}
		var by uuid.UUID
		if t.CreatedBy.Valid {
			by = t.CreatedBy.UUID
		}
		if _, _, err := s.queue.Enqueue(ctx, queue.NewTask{
			Kind: KindMatchSource, Payload: matchSourcePayload{WorkID: p.WorkID, SourceID: src.ID},
			DedupeKey: "match:" + p.WorkID.String() + ":" + src.ID, LimitKey: "source:" + src.ID, CreatedBy: by, Priority: 10,
		}); err != nil {
			return nil, err
		}
		queued++
	}
	s.notify(ctx, p.WorkID)
	return map[string]int{"sources": queued}, nil
}

// entryTitles lists an entry's titles to search with: the main title, then
// titles in the source's language, then romanized ones, then English, then
// the rest; normalized duplicates dropped.
func entryTitles(e *metav1.Entry, sourceLang string) []string {
	type cand struct {
		text string
		rank int
	}
	lang := strings.ToLower(strings.SplitN(sourceLang, "-", 2)[0])
	var cs []cand
	cs = append(cs, cand{e.GetTitle(), 0})
	for _, t := range e.GetTitles() {
		l := strings.ToLower(t.GetLanguage())
		base := strings.SplitN(l, "-", 2)[0]
		rank := 5
		switch {
		case t.GetIsPrimary() && lang != "" && base == lang:
			rank = 1
		case lang != "" && base == lang:
			rank = 2
		case strings.HasSuffix(l, "-latn") || t.GetKind() == metadatav1.TitleKind_TITLE_KIND_ROMANIZED:
			rank = 3
		case base == "en":
			rank = 4
		}
		cs = append(cs, cand{t.GetText(), rank})
	}
	slices.SortStableFunc(cs, func(a, b cand) int { return a.rank - b.rank })
	var out []string
	seen := map[string]bool{}
	for _, c := range cs {
		k := textmatch.Normalize(c.text)
		if k == "" || seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, c.text)
	}
	return out
}

// entryPeople lists an entry's creators (not voice actors and the like).
func entryPeople(e *metav1.Entry) []string {
	var out []string
	for _, c := range e.GetCredits() {
		switch c.GetRole() {
		case metadatav1.PersonRole_PERSON_ROLE_AUTHOR, metadatav1.PersonRole_PERSON_ROLE_ARTIST,
			metadatav1.PersonRole_PERSON_ROLE_WRITER, metadatav1.PersonRole_PERSON_ROLE_ILLUSTRATOR,
			metadatav1.PersonRole_PERSON_ROLE_ORIGINAL_CREATOR:
			p := c.GetPerson()
			out = append(out, p.GetName())
			if p.GetNameOriginal() != "" {
				out = append(out, p.GetNameOriginal())
			}
			out = append(out, p.GetAliases()...)
		}
	}
	return out
}

type candidate struct {
	series   *sourcev1.Series
	query    string
	score    float64
	matched  string
	evidence Evidence
}

// handleMatchSource searches one source for a work.
func (s *Service) handleMatchSource(ctx context.Context, t *queue.Task) (any, error) {
	var p matchSourcePayload
	if err := t.Decode(&p); err != nil {
		return nil, queue.Permanent(err)
	}
	src, err := s.host.Source(ctx, p.SourceID, adminViewer.plugins())
	if errors.Is(err, plugins.ErrNotFound) {
		return nil, queue.Permanent(fmt.Errorf("source %s is gone or turned off", p.SourceID))
	}
	if err != nil {
		return nil, err
	}
	w, err := s.st.GetWork(ctx, p.WorkID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, queue.Permanent(ErrNotFound)
	}
	if err != nil {
		return nil, err
	}
	e, err := s.meta.GetEntry(ctx, w.MetaEntryID, "", adminViewer.meta())
	if err != nil {
		return nil, err // meta down: retry later
	}
	cfg := s.matchSettings(ctx)
	titles := entryTitles(e, src.Lang)
	people := textmatch.RealNames(entryPeople(e))
	key := s.limits.For("source:" + src.ID)

	var (
		queries []string
		found   = map[string]*candidate{}
	)
	fail := func(err error) (any, error) {
		msg := err.Error()
		var pe *plugin.Error
		if errors.As(err, &pe) {
			msg = pe.Message
			if pe.Reason != "" {
				msg = string(pe.Reason) + ": " + pe.Message
			}
			if pe.Code == plugin.CodeResourceExhausted {
				err = &queue.RetryAfterError{Err: err, After: pe.RetryAfter}
			}
		}
		if serr := s.st.UpsertWorkSourceMatch(ctx, db.UpsertWorkSourceMatchParams{
			WorkID: p.WorkID, SourceID: src.ID, Status: MatchFailed, Message: msg, Queries: nonNil(queries),
		}); serr != nil {
			return nil, serr
		}
		s.notify(ctx, p.WorkID)
		return nil, err
	}
	for i, q := range titles {
		if i >= cfg.MaxQueries {
			break
		}
		if err := key.Wait(ctx); err != nil {
			return nil, err
		}
		page, err := s.host.Search(ctx, adminViewer.plugins(), src.ID, q, 1, nil)
		if err != nil {
			return fail(err)
		}
		queries = append(queries, q)
		hit := false
		for _, se := range page.Series {
			names := append([]string{se.GetTitle()}, se.GetAltTitles()...)
			score, matched, _ := textmatch.Best(names, titles)
			if score < cfg.CandidateScore {
				continue
			}
			hit = true
			if c, ok := found[se.GetKey()]; !ok || score > c.score {
				found[se.GetKey()] = &candidate{series: se, query: q, score: score, matched: matched}
			}
		}
		t.SetProgress(queue.Progress{Phase: "searching", Done: int64(i + 1), Total: int64(min(len(titles), cfg.MaxQueries)), Unit: "queries"})
		if hit {
			break // found candidates: no need for more titles
		}
	}

	cands := make([]*candidate, 0, len(found))
	for _, c := range found {
		cands = append(cands, c)
	}
	slices.SortFunc(cands, func(a, b *candidate) int {
		if a.score != b.score {
			if a.score > b.score {
				return -1
			}
			return 1
		}
		return strings.Compare(a.series.GetKey(), b.series.GetKey())
	})
	if len(cands) > cfg.MaxCandidates {
		cands = cands[:cfg.MaxCandidates]
	}

	// Full details of the best few: people (and alternative titles) and
	// how many chapters there are.
	for i, c := range cands {
		c.evidence = Evidence{EntryTitle: e.GetTitle(), MatchedTitle: c.matched, Query: c.query, TitleScore: round(c.score)}
		if i >= cfg.Details {
			continue
		}
		if err := key.Wait(ctx); err != nil {
			return nil, err
		}
		full, err := s.host.Series(ctx, adminViewer.plugins(), src.ID, c.series.GetKey())
		if err != nil {
			return fail(err)
		}
		c.series = mergeSeries(c.series, full)
		names := append([]string{full.GetTitle()}, full.GetAltTitles()...)
		if score, matched, _ := textmatch.Best(names, titles); score > c.score {
			c.score, c.evidence.TitleScore, c.evidence.MatchedTitle = score, round(score), matched
		}
		srcPeople := append(slices.Clone(full.GetAuthors()), full.GetArtists()...)
		c.evidence.SourceAuthors = srcPeople
		if real := textmatch.RealNames(srcPeople); len(real) > 0 && len(people) > 0 {
			overlap := textmatch.PeopleOverlap(people, real)
			c.evidence.AuthorsMatch = &overlap
		}
		if err := key.Wait(ctx); err != nil {
			return nil, err
		}
		if chs, err := s.host.Chapters(ctx, adminViewer.plugins(), src.ID, c.series.GetKey()); err == nil {
			n := len(chs)
			c.evidence.ChapterCount = &n
		}
	}

	// Strong: the exact title and a shared author. Auto-attach only when
	// exactly one result is strong (two strong results need a person).
	var strong []*candidate
	for _, c := range cands {
		if c.score >= 1 && c.evidence.AuthorsMatch != nil && *c.evidence.AuthorsMatch {
			strong = append(strong, c)
		}
	}
	status, attached, err := s.saveMatches(ctx, p.WorkID, src.ID, cands, strong)
	if err != nil {
		return nil, err
	}
	msg := fmt.Sprintf("%d candidates", len(cands))
	if attached != nil {
		msg = "attached " + attached.Title
		s.hooks.SeriesAttached(ctx, *attached)
	}
	if err := s.st.UpsertWorkSourceMatch(ctx, db.UpsertWorkSourceMatchParams{
		WorkID: p.WorkID, SourceID: src.ID, Status: status, Message: msg, Queries: nonNil(queries),
	}); err != nil {
		return nil, err
	}
	s.notify(ctx, p.WorkID)
	return map[string]any{"status": status, "candidates": len(cands)}, nil
}

// saveMatches replaces a source's candidates for a work and attaches the
// single strong one, unless a series is attached already or the strong
// one was rejected before.
func (s *Service) saveMatches(ctx context.Context, workID uuid.UUID, sourceID string, cands, strong []*candidate) (status string, attached *db.SourceSeries, err error) {
	return s.saveMatchesPaused(ctx, workID, sourceID, cands, strong, nil)
}

// saveMatchesPaused is saveMatches; tests pass pause to run something
// between dropping the old candidates and saving the new ones.
func (s *Service) saveMatchesPaused(ctx context.Context, workID uuid.UUID, sourceID string, cands, strong []*candidate, pause func()) (status string, attached *db.SourceSeries, err error) {
	err = s.st.InTx(ctx, func(q *db.Queries, _ pgx.Tx) error {
		// The work's lock first, as every writer of its series takes it:
		// without it, attaching one of these candidates by hand meanwhile
		// could deadlock with this (the attach holds the work and waits on
		// a dropped candidate; the insert below waits on the work).
		if err := q.LockWork(ctx, workID); err != nil {
			return err
		}
		if err := q.DropCandidates(ctx, db.DropCandidatesParams{WorkID: workID, SourceID: sourceID}); err != nil {
			return err
		}
		if pause != nil {
			pause()
		}
		current, err := q.AttachedSourceSeries(ctx, workID)
		if err != nil {
			return err
		}
		hasAttached := slices.ContainsFunc(current, func(se db.SourceSeries) bool { return se.SourceID == sourceID })
		for _, c := range cands {
			row, err := q.UpsertSourceSeries(ctx, db.UpsertSourceSeriesParams{
				WorkID: workID, SourceID: sourceID, Key: c.series.GetKey(), Title: c.series.GetTitle(),
				Url: c.series.GetUrl(), CoverRef: c.series.GetCoverUrl(), State: StateCandidate, Origin: OriginAuto,
				Score: float32(c.score), Evidence: c.evidence.json(),
			})
			if err != nil {
				return err
			}
			if len(strong) == 1 && strong[0] == c && !hasAttached && row.State == StateCandidate {
				row, err = q.SetSourceSeriesState(ctx, db.SetSourceSeriesStateParams{ID: row.ID, State: StateAttached, Origin: OriginAuto})
				if err != nil {
					return err
				}
				hasAttached, attached = true, &row
			}
		}
		switch {
		case hasAttached:
			status = MatchMatched
		case len(cands) > 0:
			status = MatchCandidates
		default:
			status = MatchNone
		}
		return nil
	})
	return status, attached, err
}

// mergeSeries keeps listing fields the details lack.
func mergeSeries(listed, full *sourcev1.Series) *sourcev1.Series {
	if full == nil {
		return listed
	}
	if full.GetKey() == "" {
		full.Key = listed.GetKey()
	}
	if full.GetCoverUrl() == "" {
		full.CoverUrl = listed.GetCoverUrl()
	}
	if full.GetUrl() == "" {
		full.Url = listed.GetUrl()
	}
	return full
}

func round(f float64) float64 { return float64(int(f*1000+0.5)) / 1000 }

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

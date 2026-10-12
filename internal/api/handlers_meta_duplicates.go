// SPDX-License-Identifier: GPL-3.0-or-later

package api

import (
	"context"
	"errors"
	"net/http"
	"strings"

	metav1 "github.com/panelarr/meta/gen/panelarr/meta/v1"

	"github.com/panelarr/server/internal/api/apigen"
	"github.com/panelarr/server/internal/meta"
)

// The metadata service's duplicate entries review, and its decision model.

func metaReviewError(err error) error {
	var bad *meta.BadRequestError
	switch {
	case errors.Is(err, meta.ErrPairNotFound):
		return NewProblem(http.StatusNotFound, CodeNotFound, "no such pair")
	case errors.Is(err, meta.ErrPairDecided):
		return NewProblem(http.StatusConflict, "pair_decided", "the pair can't take that decision now")
	case errors.As(err, &bad):
		return NewProblem(http.StatusBadRequest, CodeInvalidRequest, bad.Reason)
	}
	return metaError(err)
}

func toDuplicateVerdict(v *metav1.DuplicateVerdict) *apigen.DuplicateVerdict {
	if v == nil {
		return nil
	}
	out := &apigen.DuplicateVerdict{
		Judge: v.GetJudge(), Model: nonEmpty(v.GetModel()), Same: float32(v.GetSame()), Choice: v.GetChoice(),
		Probabilities: map[string]float32{}, Confidence: ptr(float32(v.GetConfidence())), Tokens: ptr(v.GetTokens()),
	}
	for k, p := range v.GetProbabilities() {
		out.Probabilities[k] = float32(p)
	}
	if v.GetAt() != nil {
		at := v.GetAt().AsTime()
		out.At = &at
	}
	return out
}

func nonZero[T comparable](v T) *T {
	var zero T
	if v == zero {
		return nil
	}
	return &v
}

func toDuplicatePair(p *metav1.DuplicatePair) apigen.DuplicatePair {
	status := enumName(p.GetStatus().String(), "DUPLICATE_STATUS_")
	if status == "unspecified" {
		status = "unknown"
	}
	sig := p.GetSignals()
	out := apigen.DuplicatePair{
		Id: p.GetId(), EntryA: toSummary(p.GetEntryA()), EntryB: toSummary(p.GetEntryB()),
		Status: apigen.DuplicatePairStatus(status), Score: float32(p.GetScore()), Verdict: toDuplicateVerdict(p.GetVerdict()),
		DecidedBy: nonEmpty(p.GetDecidedBy()), Note: nonEmpty(p.GetNote()),
		Signals: apigen.DuplicateSignals{
			Title: float32(sig.GetTitle()), TitleA: nonEmpty(sig.GetTitleA()), TitleB: nonEmpty(sig.GetTitleB()),
			People: float32(sig.GetPeople()), SharedPeople: nonNil(sig.GetSharedPeople()),
			YearA: nonZero(int(sig.GetYearA())), YearB: nonZero(int(sig.GetYearB())),
			TypeA: nonEmpty(sig.GetTypeA()), TypeB: nonEmpty(sig.GetTypeB()),
			LanguageA: nonEmpty(sig.GetLanguageA()), LanguageB: nonEmpty(sig.GetLanguageB()),
			SharedIds: nonNil(sig.GetSharedIds()), SameProvider: sig.GetSameProvider(),
		},
	}
	if t := p.GetCreatedAt(); t != nil {
		out.CreatedAt = t.AsTime()
	}
	if t := p.GetUpdatedAt(); t != nil {
		out.UpdatedAt = t.AsTime()
	}
	if t := p.GetDecidedAt(); t != nil {
		at := t.AsTime()
		out.DecidedAt = &at
	}
	return out
}

// ListMetaDuplicates lists suspected duplicate pairs.
func (s *Server) ListMetaDuplicates(ctx context.Context, req apigen.ListMetaDuplicatesRequestObject) (apigen.ListMetaDuplicatesResponseObject, error) {
	pr := req.Params
	q := meta.DuplicateQuery{
		JudgedOnly: deref(pr.JudgedOnly), PageSize: int32(min(pageSize(pr.PageSize), 100)), PageToken: deref(pr.PageToken), //nolint:gosec // at most 100
	}
	if pr.Status != nil {
		q.Status = metav1.DuplicateStatus(metav1.DuplicateStatus_value["DUPLICATE_STATUS_"+strings.ToUpper(string(*pr.Status))])
	}
	if pr.MinScore != nil {
		q.MinScore = float64(*pr.MinScore)
	}
	res, err := s.deps.Meta.Duplicates(ctx, q)
	if err != nil {
		return nil, metaReviewError(err)
	}
	out := apigen.ListMetaDuplicates200JSONResponse{
		Pairs: make([]apigen.DuplicatePair, len(res.GetPairs())), NextPageToken: res.GetNextPageToken(), TotalSize: res.GetTotalSize(),
	}
	for i, p := range res.GetPairs() {
		out.Pairs[i] = toDuplicatePair(p)
	}
	return out, nil
}

// GetMetaDuplicate returns one pair.
func (s *Server) GetMetaDuplicate(ctx context.Context, req apigen.GetMetaDuplicateRequestObject) (apigen.GetMetaDuplicateResponseObject, error) {
	p, err := s.deps.Meta.Duplicate(ctx, req.PairId)
	if err != nil {
		return nil, metaReviewError(err)
	}
	return apigen.GetMetaDuplicate200JSONResponse(toDuplicatePair(p)), nil
}

// ConfirmMetaDuplicate merges a pair.
func (s *Server) ConfirmMetaDuplicate(ctx context.Context, req apigen.ConfirmMetaDuplicateRequestObject) (apigen.ConfirmMetaDuplicateResponseObject, error) {
	keep, note := "", ""
	if b := req.Body; b != nil {
		if b.Keep != nil {
			keep = string(*b.Keep)
		}
		note = deref(b.Note)
	}
	p, entry, err := s.deps.Meta.ConfirmDuplicate(ctx, req.PairId, keep, note)
	if err != nil {
		return nil, metaReviewError(err)
	}
	s.deps.Log.InfoContext(ctx, "duplicate entries merged", "component", "audit", "pair", req.PairId, "entry", entry)
	return apigen.ConfirmMetaDuplicate200JSONResponse{Pair: toDuplicatePair(p), EntryId: entry}, nil
}

// DismissMetaDuplicate marks a pair as different works.
func (s *Server) DismissMetaDuplicate(ctx context.Context, req apigen.DismissMetaDuplicateRequestObject) (apigen.DismissMetaDuplicateResponseObject, error) {
	note := ""
	if req.Body != nil {
		note = deref(req.Body.Note)
	}
	p, err := s.deps.Meta.DismissDuplicate(ctx, req.PairId, note)
	if err != nil {
		return nil, metaReviewError(err)
	}
	s.deps.Log.InfoContext(ctx, "duplicate pair dismissed", "component", "audit", "pair", req.PairId)
	return apigen.DismissMetaDuplicate200JSONResponse(toDuplicatePair(p)), nil
}

// ReopenMetaDuplicate takes back a dismissal.
func (s *Server) ReopenMetaDuplicate(ctx context.Context, req apigen.ReopenMetaDuplicateRequestObject) (apigen.ReopenMetaDuplicateResponseObject, error) {
	p, err := s.deps.Meta.ReopenDuplicate(ctx, req.PairId)
	if err != nil {
		return nil, metaReviewError(err)
	}
	s.deps.Log.InfoContext(ctx, "duplicate pair reopened", "component", "audit", "pair", req.PairId)
	return apigen.ReopenMetaDuplicate200JSONResponse(toDuplicatePair(p)), nil
}

func toJudge(st *metav1.DecisionJudgeSettings) apigen.DuplicateJudge {
	return apigen.DuplicateJudge{
		Enabled: st.GetEnabled(), Kind: st.GetKind(), Url: nonEmpty(st.GetUrl()), Model: nonEmpty(st.GetModel()),
		HasApiKey: st.GetHasApiKey(), Covers: st.GetCovers(), Origin: st.GetOrigin(),
	}
}

func judgeSettings(b apigen.DuplicateJudgeSettings) *metav1.DecisionJudgeSettings {
	return &metav1.DecisionJudgeSettings{
		Enabled: b.Enabled, Kind: b.Kind, Url: deref(b.Url), Model: deref(b.Model), ApiKey: deref(b.ApiKey), Covers: deref(b.Covers),
	}
}

// GetMetaDuplicateJudge returns the decision model settings.
func (s *Server) GetMetaDuplicateJudge(ctx context.Context, _ apigen.GetMetaDuplicateJudgeRequestObject) (apigen.GetMetaDuplicateJudgeResponseObject, error) {
	st, err := s.deps.Meta.Judge(ctx)
	if err != nil {
		return nil, metaReviewError(err)
	}
	return apigen.GetMetaDuplicateJudge200JSONResponse(toJudge(st)), nil
}

// PutMetaDuplicateJudge saves the decision model settings.
func (s *Server) PutMetaDuplicateJudge(ctx context.Context, req apigen.PutMetaDuplicateJudgeRequestObject) (apigen.PutMetaDuplicateJudgeResponseObject, error) {
	st, err := s.deps.Meta.PutJudge(ctx, judgeSettings(*req.Body))
	if err != nil {
		return nil, metaReviewError(err)
	}
	s.deps.Log.InfoContext(ctx, "duplicate judge changed", "component", "audit", "enabled", st.GetEnabled(), "kind", st.GetKind())
	return apigen.PutMetaDuplicateJudge200JSONResponse(toJudge(st)), nil
}

// TestMetaDuplicateJudge tries decision model settings.
func (s *Server) TestMetaDuplicateJudge(ctx context.Context, req apigen.TestMetaDuplicateJudgeRequestObject) (apigen.TestMetaDuplicateJudgeResponseObject, error) {
	res, err := s.deps.Meta.TestJudge(ctx, judgeSettings(*req.Body))
	if err != nil {
		return nil, metaReviewError(err)
	}
	return apigen.TestMetaDuplicateJudge200JSONResponse{Ok: res.GetOk(), Error: nonEmpty(res.GetError()), Verdict: toDuplicateVerdict(res.GetVerdict())}, nil
}

// ResetMetaDuplicateJudge goes back to meta's configured settings.
func (s *Server) ResetMetaDuplicateJudge(ctx context.Context, _ apigen.ResetMetaDuplicateJudgeRequestObject) (apigen.ResetMetaDuplicateJudgeResponseObject, error) {
	st, err := s.deps.Meta.ResetJudge(ctx)
	if err != nil {
		return nil, metaReviewError(err)
	}
	s.deps.Log.InfoContext(ctx, "duplicate judge reset", "component", "audit")
	return apigen.ResetMetaDuplicateJudge200JSONResponse(toJudge(st)), nil
}

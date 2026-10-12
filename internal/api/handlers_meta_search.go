// SPDX-License-Identifier: GPL-3.0-or-later

package api

import (
	"context"

	metav1 "github.com/panelarr/meta/gen/panelarr/meta/v1"

	"github.com/panelarr/server/internal/api/apigen"
)

// The metadata service's search engine settings and index state.

func toMetaSearchEngine(st *metav1.SearchEngineSettings, ss *metav1.SearchEngineStatus) apigen.MetaSearchEngine {
	var out apigen.MetaSearchEngine
	out.Settings.Backend, out.Settings.Url, out.Settings.HasApiKey = st.GetBackend(), nonEmpty(st.GetUrl()), st.GetHasApiKey()
	out.Settings.IndexPrefix, out.Settings.Browse, out.Settings.Origin = nonEmpty(st.GetIndexPrefix()), nonEmpty(st.GetBrowse()), st.GetOrigin()
	out.Status = apigen.SearchEngineStatus{
		Backend: ss.GetBackend(), TypoTolerance: ss.GetTypoTolerance(), Substring: ss.GetSubstring(), Facets: ss.GetFacets(),
		Ready: ss.GetReady(), Reachable: ss.GetReachable(), Error: nonEmpty(ss.GetError()), Degraded: ss.GetDegraded(),
		Index: nonEmpty(ss.GetIndex()), Documents: ss.GetDocuments(), Pending: ss.GetPending(), Applied: ss.GetApplied(),
		IndexerError: nonEmpty(ss.GetIndexerError()),
	}
	if t := ss.GetErrorAt(); t != nil {
		at := t.AsTime()
		out.Status.ErrorAt = &at
	}
	if t := ss.GetBuiltAt(); t != nil {
		at := t.AsTime()
		out.Status.BuiltAt = &at
	}
	if t := ss.GetIndexerErrorAt(); t != nil {
		at := t.AsTime()
		out.Status.IndexerErrorAt = &at
	}
	if l := ss.GetLag(); l != nil {
		out.Status.LagSeconds = ptr(float32(l.AsDuration().Seconds()))
	}
	return out
}

func searchSettings(b apigen.SearchEngineSettings) *metav1.SearchEngineSettings {
	out := &metav1.SearchEngineSettings{Backend: string(b.Backend), Url: deref(b.Url), ApiKey: deref(b.ApiKey), IndexPrefix: deref(b.IndexPrefix)}
	if b.Browse != nil {
		out.Browse = string(*b.Browse)
	}
	return out
}

// GetMetaSearchEngine returns meta's search engine settings and state.
func (s *Server) GetMetaSearchEngine(ctx context.Context, _ apigen.GetMetaSearchEngineRequestObject) (apigen.GetMetaSearchEngineResponseObject, error) {
	st, ss, err := s.deps.Meta.SearchEngine(ctx)
	if err != nil {
		return nil, metaReviewError(err)
	}
	return apigen.GetMetaSearchEngine200JSONResponse(toMetaSearchEngine(st, ss)), nil
}

// PutMetaSearchEngine saves meta's search engine settings.
func (s *Server) PutMetaSearchEngine(ctx context.Context, req apigen.PutMetaSearchEngineRequestObject) (apigen.PutMetaSearchEngineResponseObject, error) {
	st, ss, err := s.deps.Meta.PutSearchEngine(ctx, searchSettings(*req.Body))
	if err != nil {
		return nil, metaReviewError(err)
	}
	s.deps.Log.InfoContext(ctx, "meta search engine changed", "component", "audit", "backend", st.GetBackend())
	return apigen.PutMetaSearchEngine200JSONResponse(toMetaSearchEngine(st, ss)), nil
}

// TestMetaSearchEngine tries search engine settings.
func (s *Server) TestMetaSearchEngine(ctx context.Context, req apigen.TestMetaSearchEngineRequestObject) (apigen.TestMetaSearchEngineResponseObject, error) {
	ok, msg, err := s.deps.Meta.TestSearchEngine(ctx, searchSettings(*req.Body))
	if err != nil {
		return nil, metaReviewError(err)
	}
	return apigen.TestMetaSearchEngine200JSONResponse{Ok: ok, Error: nonEmpty(msg)}, nil
}

// ResetMetaSearchEngine goes back to meta's configured search engine.
func (s *Server) ResetMetaSearchEngine(ctx context.Context, _ apigen.ResetMetaSearchEngineRequestObject) (apigen.ResetMetaSearchEngineResponseObject, error) {
	st, ss, err := s.deps.Meta.ResetSearchEngine(ctx)
	if err != nil {
		return nil, metaReviewError(err)
	}
	s.deps.Log.InfoContext(ctx, "meta search engine reset", "component", "audit")
	return apigen.ResetMetaSearchEngine200JSONResponse(toMetaSearchEngine(st, ss)), nil
}

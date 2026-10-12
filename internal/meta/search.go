// SPDX-License-Identifier: GPL-3.0-or-later

package meta

import (
	"context"

	"connectrpc.com/connect"

	metav1 "github.com/panelarr/meta/gen/panelarr/meta/v1"
)

// The metadata service's search engine: its settings (the API key is
// never returned) and the state of its index.

// SearchEngine returns the search engine settings in force and its state.
func (s *Service) SearchEngine(ctx context.Context) (*metav1.SearchEngineSettings, *metav1.SearchEngineStatus, error) {
	c, err := s.Client()
	if err != nil {
		return nil, nil, err
	}
	res, err := c.Integrations.GetSearchEngine(ctx, connect.NewRequest(&metav1.GetSearchEngineRequest{}))
	if err != nil {
		return nil, nil, reviewError("get search engine", err)
	}
	return res.Msg.GetSettings(), res.Msg.GetStatus(), nil
}

// PutSearchEngine saves search engine settings; an empty API key keeps
// the stored one.
func (s *Service) PutSearchEngine(ctx context.Context, st *metav1.SearchEngineSettings) (*metav1.SearchEngineSettings, *metav1.SearchEngineStatus, error) {
	c, err := s.Client()
	if err != nil {
		return nil, nil, err
	}
	res, err := c.Integrations.PutSearchEngine(ctx, connect.NewRequest(&metav1.PutSearchEngineRequest{Settings: st}))
	if err != nil {
		return nil, nil, reviewError("put search engine", err)
	}
	return res.Msg.GetSettings(), res.Msg.GetStatus(), nil
}

// TestSearchEngine tries search engine settings without saving them.
func (s *Service) TestSearchEngine(ctx context.Context, st *metav1.SearchEngineSettings) (bool, string, error) {
	c, err := s.Client()
	if err != nil {
		return false, "", err
	}
	res, err := c.Integrations.TestSearchEngine(ctx, connect.NewRequest(&metav1.TestSearchEngineRequest{Settings: st}))
	if err != nil {
		return false, "", reviewError("test search engine", err)
	}
	return res.Msg.GetOk(), res.Msg.GetError(), nil
}

// ResetSearchEngine forgets the admin's settings (meta's configuration
// applies again).
func (s *Service) ResetSearchEngine(ctx context.Context) (*metav1.SearchEngineSettings, *metav1.SearchEngineStatus, error) {
	c, err := s.Client()
	if err != nil {
		return nil, nil, err
	}
	res, err := c.Integrations.ResetSearchEngine(ctx, connect.NewRequest(&metav1.ResetSearchEngineRequest{}))
	if err != nil {
		return nil, nil, reviewError("reset search engine", err)
	}
	return res.Msg.GetSettings(), res.Msg.GetStatus(), nil
}

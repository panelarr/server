// SPDX-License-Identifier: GPL-3.0-or-later

package meta

import (
	"context"
	"errors"

	"connectrpc.com/connect"

	metav1 "github.com/panelarr/meta/gen/panelarr/meta/v1"
)

// The metadata service's own admin, for Panelarr's admin: providers, their
// sync runs, the service's health, and refreshing one entry.

// ErrRunNotFound is a sync run meta doesn't know.
var ErrRunNotFound = errors.New("meta: no such sync run")

// Providers lists meta's providers with their plugin, health, schedules,
// record counts and last runs.
func (s *Service) Providers(ctx context.Context) ([]*metav1.Provider, error) {
	c, err := s.Client()
	if err != nil {
		return nil, err
	}
	res, err := c.Admin.ListProviders(ctx, connect.NewRequest(&metav1.ListProvidersRequest{}))
	if err != nil {
		return nil, wrap("list providers", err)
	}
	return res.Msg.GetProviders(), nil
}

// SyncRuns lists sync runs, newest first, of one provider or all.
func (s *Service) SyncRuns(ctx context.Context, provider string, pageSize int32, token string) ([]*metav1.SyncRun, string, error) {
	c, err := s.Client()
	if err != nil {
		return nil, "", err
	}
	res, err := c.Admin.ListSyncRuns(ctx, connect.NewRequest(&metav1.ListSyncRunsRequest{Provider: provider, PageSize: pageSize, PageToken: token}))
	if err != nil {
		return nil, "", wrap("list sync runs", err)
	}
	return res.Msg.GetRuns(), res.Msg.GetNextPageToken(), nil
}

// SyncRun returns one sync run in full.
func (s *Service) SyncRun(ctx context.Context, id string) (*metav1.SyncRun, error) {
	c, err := s.Client()
	if err != nil {
		return nil, err
	}
	res, err := c.Admin.GetSyncRun(ctx, connect.NewRequest(&metav1.GetSyncRunRequest{Id: id}))
	if connect.CodeOf(err) == connect.CodeNotFound && connect.IsWireError(err) {
		return nil, ErrRunNotFound
	}
	if err != nil {
		return nil, wrap("get sync run", err)
	}
	return res.Msg.GetRun(), nil
}

// Health is meta's own health, per component.
func (s *Service) Health(ctx context.Context) (*metav1.GetHealthResponse, error) {
	c, err := s.Client()
	if err != nil {
		return nil, err
	}
	res, err := c.Admin.GetHealth(ctx, connect.NewRequest(&metav1.GetHealthRequest{}))
	if err != nil {
		return nil, wrap("health", err)
	}
	return res.Msg, nil
}

// RefreshEntry asks every provider of an entry for fresh data now.
func (s *Service) RefreshEntry(ctx context.Context, id string) (*metav1.RefreshEntryResponse, error) {
	c, err := s.Client()
	if err != nil {
		return nil, err
	}
	res, err := c.Admin.RefreshEntry(ctx, connect.NewRequest(&metav1.RefreshEntryRequest{Id: id}))
	if connect.CodeOf(err) == connect.CodeNotFound && connect.IsWireError(err) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, wrap("refresh entry", err)
	}
	return res.Msg, nil
}

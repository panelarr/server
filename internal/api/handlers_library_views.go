// SPDX-License-Identifier: GPL-3.0-or-later

package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/google/uuid"

	"github.com/panelarr/server/internal/api/apigen"
	"github.com/panelarr/server/internal/auth"
	"github.com/panelarr/server/internal/library"
)

// viewError maps saved view errors. Another user's view is answered like a
// missing one, so its existence isn't revealed.
func viewError(err error) error {
	var bad *library.ViewError
	switch {
	case err == nil:
		return nil
	case errors.As(err, &bad):
		return fieldProblem(bad.Field, bad.Reason)
	case errors.Is(err, library.ErrNotFound):
		return NewProblem(http.StatusNotFound, CodeNotFound, "no such view")
	case errors.Is(err, library.ErrViewNameTaken):
		return NewProblem(http.StatusConflict, "name_taken", "you have a view with this name")
	case errors.Is(err, library.ErrViewLimit):
		return NewProblem(http.StatusConflict, "view_limit", err.Error())
	case errors.Is(err, library.ErrDefaultRequired):
		return NewProblem(http.StatusConflict, "default_required", "one view must be the default: make another view the default instead")
	case errors.Is(err, library.ErrLastView):
		return NewProblem(http.StatusConflict, "last_view", "your only view can't be deleted")
	case errors.Is(err, library.ErrOrderMismatch):
		return NewProblem(http.StatusConflict, "order_mismatch", "list each of your views exactly once")
	}
	return err
}

func toLibraryView(v library.View) (apigen.LibraryView, error) {
	out := apigen.LibraryView{
		Id: v.ID, Name: v.Name, Position: v.Position, Default: v.Default, CreatedAt: v.CreatedAt, UpdatedAt: v.UpdatedAt,
	}
	if err := json.Unmarshal(v.Definition, &out.Definition); err != nil {
		return apigen.LibraryView{}, err
	}
	return out, nil
}

func toLibraryViewList(vs []library.View) (apigen.LibraryViewList, error) {
	out := apigen.LibraryViewList{Views: make([]apigen.LibraryView, len(vs))}
	for i, v := range vs {
		lv, err := toLibraryView(v)
		if err != nil {
			return apigen.LibraryViewList{}, err
		}
		out.Views[i] = lv
	}
	return out, nil
}

func callerID(ctx context.Context) uuid.UUID { return auth.FromContext(ctx).User.ID }

// ListMyLibraryViews lists the caller's saved views.
func (s *Server) ListMyLibraryViews(ctx context.Context, _ apigen.ListMyLibraryViewsRequestObject) (apigen.ListMyLibraryViewsResponseObject, error) {
	vs, err := s.deps.Library.Views(ctx, callerID(ctx))
	if err != nil {
		return nil, err
	}
	out, err := toLibraryViewList(vs)
	if err != nil {
		return nil, err
	}
	return apigen.ListMyLibraryViews200JSONResponse(out), nil
}

// CreateMyLibraryView saves a view.
func (s *Server) CreateMyLibraryView(ctx context.Context, req apigen.CreateMyLibraryViewRequestObject) (apigen.CreateMyLibraryViewResponseObject, error) {
	def, err := json.Marshal(req.Body.Definition)
	if err != nil {
		return nil, err
	}
	v, err := s.deps.Library.CreateView(ctx, callerID(ctx), req.Body.Name, def, deref(req.Body.Default))
	if err != nil {
		return nil, viewError(err)
	}
	out, err := toLibraryView(v)
	if err != nil {
		return nil, err
	}
	return apigen.CreateMyLibraryView201JSONResponse(out), nil
}

// GetMyLibraryView returns one of the caller's views.
func (s *Server) GetMyLibraryView(ctx context.Context, req apigen.GetMyLibraryViewRequestObject) (apigen.GetMyLibraryViewResponseObject, error) {
	v, err := s.deps.Library.View(ctx, callerID(ctx), req.Id)
	if err != nil {
		return nil, viewError(err)
	}
	out, err := toLibraryView(v)
	if err != nil {
		return nil, err
	}
	return apigen.GetMyLibraryView200JSONResponse(out), nil
}

// UpdateMyLibraryView changes one of the caller's views.
func (s *Server) UpdateMyLibraryView(ctx context.Context, req apigen.UpdateMyLibraryViewRequestObject) (apigen.UpdateMyLibraryViewResponseObject, error) {
	p := library.ViewPatch{Name: req.Body.Name, Default: req.Body.Default}
	if req.Body.Definition != nil {
		def, err := json.Marshal(req.Body.Definition)
		if err != nil {
			return nil, err
		}
		p.Definition = def
	}
	v, err := s.deps.Library.UpdateView(ctx, callerID(ctx), req.Id, p)
	if err != nil {
		return nil, viewError(err)
	}
	out, err := toLibraryView(v)
	if err != nil {
		return nil, err
	}
	return apigen.UpdateMyLibraryView200JSONResponse(out), nil
}

// DeleteMyLibraryView deletes one of the caller's views.
func (s *Server) DeleteMyLibraryView(ctx context.Context, req apigen.DeleteMyLibraryViewRequestObject) (apigen.DeleteMyLibraryViewResponseObject, error) {
	if err := s.deps.Library.DeleteView(ctx, callerID(ctx), req.Id); err != nil {
		return nil, viewError(err)
	}
	return apigen.DeleteMyLibraryView204Response{}, nil
}

// OrderMyLibraryViews reorders the caller's views.
func (s *Server) OrderMyLibraryViews(ctx context.Context, req apigen.OrderMyLibraryViewsRequestObject) (apigen.OrderMyLibraryViewsResponseObject, error) {
	vs, err := s.deps.Library.OrderViews(ctx, callerID(ctx), req.Body.Ids)
	if err != nil {
		return nil, viewError(err)
	}
	out, err := toLibraryViewList(vs)
	if err != nil {
		return nil, err
	}
	return apigen.OrderMyLibraryViews200JSONResponse(out), nil
}

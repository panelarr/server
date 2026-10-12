// SPDX-License-Identifier: GPL-3.0-or-later

package meta

import (
	"context"
	"errors"

	"connectrpc.com/connect"

	metav1 "github.com/panelarr/meta/gen/panelarr/meta/v1"
)

// The review of duplicate catalogue entries (pairs meta suspects are one
// work), and the decision model that rates pairs.

// Duplicate review errors.
var (
	ErrPairNotFound = errors.New("meta: no such duplicate pair")
	// ErrPairDecided is a decision a pair's state doesn't allow (e.g.
	// reopening a confirmed pair).
	ErrPairDecided = errors.New("meta: the pair can't take that decision now")
)

// BadRequestError is a request meta refused, with its reason.
type BadRequestError struct{ Reason string }

func (e *BadRequestError) Error() string { return "meta: " + e.Reason }

func reviewError(op string, err error) error {
	if connect.IsWireError(err) {
		switch connect.CodeOf(err) {
		case connect.CodeNotFound:
			return ErrPairNotFound
		case connect.CodeFailedPrecondition:
			return ErrPairDecided
		case connect.CodeInvalidArgument:
			var ce *connect.Error
			if errors.As(err, &ce) {
				return &BadRequestError{Reason: ce.Message()}
			}
		}
	}
	return wrap(op, err)
}

// DuplicateQuery narrows the review list.
type DuplicateQuery struct {
	Status     metav1.DuplicateStatus
	MinScore   float64
	JudgedOnly bool
	PageSize   int32
	PageToken  string
}

// Duplicates lists suspected duplicate pairs.
func (s *Service) Duplicates(ctx context.Context, q DuplicateQuery) (*metav1.ListDuplicatesResponse, error) {
	c, err := s.Client()
	if err != nil {
		return nil, err
	}
	res, err := c.Duplicates.ListDuplicates(ctx, connect.NewRequest(&metav1.ListDuplicatesRequest{
		Status: q.Status, MinScore: q.MinScore, JudgedOnly: q.JudgedOnly, PageSize: q.PageSize, PageToken: q.PageToken,
	}))
	if err != nil {
		return nil, reviewError("list duplicates", err)
	}
	return res.Msg, nil
}

// Duplicate returns one pair.
func (s *Service) Duplicate(ctx context.Context, id int64) (*metav1.DuplicatePair, error) {
	c, err := s.Client()
	if err != nil {
		return nil, err
	}
	res, err := c.Duplicates.GetDuplicate(ctx, connect.NewRequest(&metav1.GetDuplicateRequest{Id: id}))
	if err != nil {
		return nil, reviewError("get duplicate", err)
	}
	return res.Msg.GetPair(), nil
}

// ConfirmDuplicate merges a pair into one entry (it can't be undone):
// keep is "a", "b" or empty for the more popular one. It returns the pair
// and the entry they are now.
func (s *Service) ConfirmDuplicate(ctx context.Context, id int64, keep, note string) (*metav1.DuplicatePair, string, error) {
	c, err := s.Client()
	if err != nil {
		return nil, "", err
	}
	res, err := c.Duplicates.ConfirmDuplicate(ctx, connect.NewRequest(&metav1.ConfirmDuplicateRequest{Id: id, Keep: keep, Note: note}))
	if err != nil {
		return nil, "", reviewError("confirm duplicate", err)
	}
	return res.Msg.GetPair(), res.Msg.GetEntryId(), nil
}

// DismissDuplicate records that a pair are different works.
func (s *Service) DismissDuplicate(ctx context.Context, id int64, note string) (*metav1.DuplicatePair, error) {
	c, err := s.Client()
	if err != nil {
		return nil, err
	}
	res, err := c.Duplicates.DismissDuplicate(ctx, connect.NewRequest(&metav1.DismissDuplicateRequest{Id: id, Note: note}))
	if err != nil {
		return nil, reviewError("dismiss duplicate", err)
	}
	return res.Msg.GetPair(), nil
}

// ReopenDuplicate takes back a dismissal.
func (s *Service) ReopenDuplicate(ctx context.Context, id int64) (*metav1.DuplicatePair, error) {
	c, err := s.Client()
	if err != nil {
		return nil, err
	}
	res, err := c.Duplicates.ReopenDuplicate(ctx, connect.NewRequest(&metav1.ReopenDuplicateRequest{Id: id}))
	if err != nil {
		return nil, reviewError("reopen duplicate", err)
	}
	return res.Msg.GetPair(), nil
}

// Judge returns the decision model settings in force (no API key).
func (s *Service) Judge(ctx context.Context) (*metav1.DecisionJudgeSettings, error) {
	c, err := s.Client()
	if err != nil {
		return nil, err
	}
	res, err := c.Duplicates.GetDecisionJudge(ctx, connect.NewRequest(&metav1.GetDecisionJudgeRequest{}))
	if err != nil {
		return nil, reviewError("get judge", err)
	}
	return res.Msg.GetSettings(), nil
}

// PutJudge saves decision model settings; an empty API key keeps meta's.
func (s *Service) PutJudge(ctx context.Context, st *metav1.DecisionJudgeSettings) (*metav1.DecisionJudgeSettings, error) {
	c, err := s.Client()
	if err != nil {
		return nil, err
	}
	res, err := c.Duplicates.PutDecisionJudge(ctx, connect.NewRequest(&metav1.PutDecisionJudgeRequest{Settings: st}))
	if err != nil {
		return nil, reviewError("put judge", err)
	}
	return res.Msg.GetSettings(), nil
}

// TestJudge asks the model about a made-up pair without saving.
func (s *Service) TestJudge(ctx context.Context, st *metav1.DecisionJudgeSettings) (*metav1.TestDecisionJudgeResponse, error) {
	c, err := s.Client()
	if err != nil {
		return nil, err
	}
	res, err := c.Duplicates.TestDecisionJudge(ctx, connect.NewRequest(&metav1.TestDecisionJudgeRequest{Settings: st}))
	if err != nil {
		return nil, reviewError("test judge", err)
	}
	return res.Msg, nil
}

// ResetJudge forgets the admin's settings (meta's configuration applies).
func (s *Service) ResetJudge(ctx context.Context) (*metav1.DecisionJudgeSettings, error) {
	c, err := s.Client()
	if err != nil {
		return nil, err
	}
	res, err := c.Duplicates.ResetDecisionJudge(ctx, connect.NewRequest(&metav1.ResetDecisionJudgeRequest{}))
	if err != nil {
		return nil, reviewError("reset judge", err)
	}
	return res.Msg.GetSettings(), nil
}

// SPDX-License-Identifier: GPL-3.0-or-later

// Package network keeps the shared network services admins set up once in
// Panelarr: challenge solvers and proxies, which network profiles use.
// Nothing uses them until an admin assigns a profile.
package network

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/panelarr/server/internal/secret"
	"github.com/panelarr/server/internal/store"
	"github.com/panelarr/server/internal/store/db"
)

// Errors.
var (
	ErrNotFound  = errors.New("network: not found")
	ErrIDTaken   = errors.New("network: the ID is taken")
	ErrInvalidID = errors.New("network: IDs are lowercase letters, digits, '-' and '_', up to 40")
	ErrInUse     = errors.New("network: in use by a network profile")
)

// InvalidError is a field with a bad value.
type InvalidError struct{ Field, Reason string }

func (e *InvalidError) Error() string { return e.Field + ": " + e.Reason }

func invalid(field, reason string) error { return &InvalidError{Field: field, Reason: reason} }

var idPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,39}$`)

// Solver types.
const (
	SolverFlareSolverr = "flaresolverr"
	SolverByparr       = "byparr"
)

// testTimeout bounds a solver or proxy test.
const testTimeout = 15 * time.Second

// Service manages solvers and proxies.
type Service struct {
	st  *store.Store
	box *secret.Box
	log *slog.Logger
	// dial is the dialer of proxy tests (tests replace it).
	dial func(ctx context.Context, network, addr string) (net.Conn, error)
	// onChange runs after anything that changes an effective profile.
	onChange func(ctx context.Context)
}

// New returns the service.
func New(st *store.Store, box *secret.Box, log *slog.Logger) *Service {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Service{st: st, box: box, log: log, dial: (&net.Dialer{Timeout: testTimeout}).DialContext}
}

// Solver is a challenge solver (FlareSolverr-compatible endpoint).
type Solver struct {
	ID, Name, Type, URL    string
	SessionTTL, MaxTimeout time.Duration
	AsResponseFallback     bool
	CreatedAt, UpdatedAt   time.Time
}

// SolverPatch changes some of a solver's fields.
type SolverPatch struct {
	Name, Type, URL        *string
	SessionTTL, MaxTimeout *time.Duration
	AsResponseFallback     *bool
}

func solverOf(r db.NetworkSolver) Solver {
	return Solver{
		ID: r.ID, Name: r.Name, Type: r.Type, URL: r.Url,
		SessionTTL: time.Duration(r.SessionTtlSeconds) * time.Second, MaxTimeout: time.Duration(r.MaxTimeoutSeconds) * time.Second,
		AsResponseFallback: r.AsResponseFallback, CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt,
	}
}

func checkSolver(s Solver) error {
	if strings.TrimSpace(s.Name) == "" {
		return invalid("name", "give it a name")
	}
	if s.Type != SolverFlareSolverr && s.Type != SolverByparr {
		return invalid("type", "flaresolverr or byparr")
	}
	if err := checkHTTPURL(s.URL); err != nil {
		return invalid("url", err.Error())
	}
	if s.SessionTTL < 0 || s.MaxTimeout < 0 {
		return invalid("session_ttl_seconds", "can't be negative")
	}
	return nil
}

func checkHTTPURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return errors.New("must be an http(s):// URL")
	}
	if u.User != nil {
		return errors.New("no credentials in the URL")
	}
	return nil
}

func seconds(d time.Duration) int32 { return int32(d / time.Second) } //nolint:gosec // bounded by the API

// Solvers lists the solvers.
func (s *Service) Solvers(ctx context.Context) ([]Solver, error) {
	rows, err := s.st.ListSolvers(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]Solver, len(rows))
	for i, r := range rows {
		out[i] = solverOf(r)
	}
	return out, nil
}

// Solver returns one solver.
func (s *Service) Solver(ctx context.Context, id string) (Solver, error) {
	r, err := s.st.GetSolver(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return Solver{}, ErrNotFound
	}
	if err != nil {
		return Solver{}, err
	}
	return solverOf(r), nil
}

// CreateSolver adds a solver.
func (s *Service) CreateSolver(ctx context.Context, in Solver) (Solver, error) {
	if !idPattern.MatchString(in.ID) {
		return Solver{}, ErrInvalidID
	}
	if err := checkSolver(in); err != nil {
		return Solver{}, err
	}
	r, err := s.st.InsertSolver(ctx, db.InsertSolverParams{
		ID: in.ID, Name: strings.TrimSpace(in.Name), Type: in.Type, Url: in.URL,
		SessionTtlSeconds: seconds(in.SessionTTL), MaxTimeoutSeconds: seconds(in.MaxTimeout), AsResponseFallback: in.AsResponseFallback,
	})
	if isUniqueViolation(err) {
		return Solver{}, ErrIDTaken
	}
	if err != nil {
		return Solver{}, err
	}
	s.log.InfoContext(ctx, "solver added", "component", "audit", "solver", in.ID)
	return solverOf(r), nil
}

// UpdateSolver changes a solver.
func (s *Service) UpdateSolver(ctx context.Context, id string, p SolverPatch) (Solver, error) {
	cur, err := s.Solver(ctx, id)
	if err != nil {
		return Solver{}, err
	}
	set(&cur.Name, p.Name)
	set(&cur.Type, p.Type)
	set(&cur.URL, p.URL)
	set(&cur.SessionTTL, p.SessionTTL)
	set(&cur.MaxTimeout, p.MaxTimeout)
	set(&cur.AsResponseFallback, p.AsResponseFallback)
	if err := checkSolver(cur); err != nil {
		return Solver{}, err
	}
	r, err := s.st.UpdateSolver(ctx, db.UpdateSolverParams{
		ID: id, Name: strings.TrimSpace(cur.Name), Type: cur.Type, Url: cur.URL,
		SessionTtlSeconds: seconds(cur.SessionTTL), MaxTimeoutSeconds: seconds(cur.MaxTimeout), AsResponseFallback: cur.AsResponseFallback,
	})
	if err != nil {
		return Solver{}, err
	}
	s.log.InfoContext(ctx, "solver changed", "component", "audit", "solver", id)
	s.changed(ctx)
	return solverOf(r), nil
}

// DeleteSolver removes a solver no profile uses.
func (s *Service) DeleteSolver(ctx context.Context, id string) error {
	n, err := s.st.DeleteSolver(ctx, id)
	if isForeignKeyViolation(err) {
		return ErrInUse
	}
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	s.log.InfoContext(ctx, "solver removed", "component", "audit", "solver", id)
	return nil
}

// TestResult is how a solver or proxy test went.
type TestResult struct {
	OK      bool
	Message string
	// Version is the solver's, when it says.
	Version string
	Latency time.Duration
}

// TestSolver checks a solver answers. It may be unsaved.
func (s *Service) TestSolver(ctx context.Context, in Solver) (TestResult, error) {
	if err := checkSolver(in); err != nil {
		return TestResult{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, testTimeout)
	defer cancel()
	start := time.Now()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, in.URL, nil)
	if err != nil {
		return TestResult{}, err
	}
	res, err := (&http.Client{Timeout: testTimeout}).Do(req)
	took := time.Since(start)
	if err != nil {
		return TestResult{Message: "no answer: " + err.Error(), Latency: took}, nil
	}
	defer res.Body.Close()
	// FlareSolverr-compatible solvers answer their root with their state.
	var info struct {
		Msg     string `json:"msg"`
		Version string `json:"version"`
	}
	_ = json.NewDecoder(io.LimitReader(res.Body, 64<<10)).Decode(&info)
	if res.StatusCode != http.StatusOK {
		return TestResult{Message: fmt.Sprintf("answered HTTP %d", res.StatusCode), Latency: took}, nil
	}
	msg := info.Msg
	if msg == "" {
		msg = "answered"
	}
	return TestResult{OK: true, Message: msg, Version: info.Version, Latency: took}, nil
}

func set[T any](dst *T, v *T) {
	if v != nil {
		*dst = *v
	}
}

func isUniqueViolation(err error) bool {
	var pg *pgconn.PgError
	return errors.As(err, &pg) && pg.Code == "23505"
}

func isForeignKeyViolation(err error) bool {
	var pg *pgconn.PgError
	return errors.As(err, &pg) && pg.Code == "23503"
}

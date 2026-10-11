// SPDX-License-Identifier: GPL-3.0-or-later

package api

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/openapi3filter"

	"github.com/panelarr/server/internal/api/apigen"
)

// Error codes shared by many endpoints. Feature code adds its own.
const (
	CodeInvalidRequest   = "invalid_request"
	CodeNotFound         = "not_found"
	CodeMethodNotAllowed = "method_not_allowed"
	CodeInternal         = "internal"
)

// Problem is an error a handler returns to send a specific problem
// response. Any other error becomes a 500 without details.
type Problem struct {
	Status int
	Code   string
	Detail string
	Fields []apigen.FieldError
}

func (p *Problem) Error() string { return p.Code + ": " + p.Detail }

// NewProblem builds a Problem.
func NewProblem(status int, code, detail string) *Problem {
	return &Problem{Status: status, Code: code, Detail: detail}
}

// writeProblem writes p as application/problem+json.
func writeProblem(w http.ResponseWriter, r *http.Request, p *Problem) {
	body := apigen.Problem{
		Type:     "about:blank",
		Title:    http.StatusText(p.Status),
		Status:   p.Status,
		Code:     p.Code,
		Instance: ptr(r.URL.Path),
	}
	if p.Detail != "" {
		body.Detail = ptr(p.Detail)
	}
	if id := RequestID(r.Context()); id != "" {
		body.RequestId = ptr(id)
	}
	if len(p.Fields) > 0 {
		body.Errors = &p.Fields
	}
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(p.Status)
	_ = json.NewEncoder(w).Encode(body)
}

// writeError turns any handler error into a problem response. Problems are
// sent as they are; anything else is logged and answered with a bare 500,
// so internal details never reach the client.
func writeError(log *slog.Logger) func(w http.ResponseWriter, r *http.Request, err error) {
	return func(w http.ResponseWriter, r *http.Request, err error) {
		var p *Problem
		if errors.As(err, &p) {
			writeProblem(w, r, p)
			return
		}
		log.ErrorContext(r.Context(), "request failed", "err", err, "method", r.Method, "path", r.URL.Path, "request_id", RequestID(r.Context()))
		writeProblem(w, r, NewProblem(http.StatusInternalServerError, CodeInternal, ""))
	}
}

// writeBadRequest answers requests the generated code couldn't decode
// (bad parameters or JSON).
func writeBadRequest(w http.ResponseWriter, r *http.Request, err error) {
	writeProblem(w, r, NewProblem(http.StatusBadRequest, CodeInvalidRequest, err.Error()))
}

func ptr[T any](v T) *T { return &v }

// validationProblem turns a spec validation failure into a 400 with one
// entry per failing field.
func validationProblem(err error) *Problem {
	var fields []apigen.FieldError
	var collect func(prefix string, err error)
	collect = func(prefix string, err error) {
		var multi openapi3.MultiError
		if errors.As(err, &multi) {
			for _, e := range multi {
				collect(prefix, e)
			}
			return
		}
		var re *openapi3filter.RequestError
		if errors.As(err, &re) {
			p := prefix
			if re.Parameter != nil {
				p = re.Parameter.Name
			}
			if re.Err != nil {
				collect(p, re.Err)
				return
			}
			fields = append(fields, apigen.FieldError{Field: p, Message: re.Reason})
			return
		}
		var se *openapi3.SchemaError
		if errors.As(err, &se) {
			field := strings.Join(se.JSONPointer(), ".")
			if prefix != "" && field != "" {
				field = prefix + "." + field
			} else if prefix != "" {
				field = prefix
			}
			fields = append(fields, apigen.FieldError{Field: field, Message: se.Reason})
			return
		}
		fields = append(fields, apigen.FieldError{Field: prefix, Message: err.Error()})
	}
	collect("", err)
	return &Problem{Status: http.StatusBadRequest, Code: CodeInvalidRequest, Detail: "the request doesn't match the API description", Fields: fields}
}

// fieldProblem is a 400 about one field.
func fieldProblem(field, message string) *Problem {
	return &Problem{Status: http.StatusBadRequest, Code: CodeInvalidRequest, Detail: message,
		Fields: []apigen.FieldError{{Field: field, Message: message}}}
}

// SPDX-License-Identifier: GPL-3.0-or-later

package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/routers"

	"github.com/panelarr/server/internal/api/apigen"
)

// Access levels, from the spec's x-panelarr-access extension.
const (
	AccessPublic = "public"
	AccessUser   = "user"
	AccessAdmin  = "admin"
)

// policy is what an operation requires, read from the spec, so the spec
// stays the single place that says who may call what.
type policy struct {
	OperationID string
	Access      string
	// Setup operations work before the first account exists.
	Setup bool
	// SessionOnly operations refuse API keys.
	SessionOnly bool
	// ReadOnly operations only read even when their method isn't GET (a
	// POST with a structured query): a key's read scope is enough.
	ReadOnly bool
	// MaxBody raises the request body limit for this operation (an
	// upload). Its body is left to the handler: neither buffered for the
	// NUL check nor validated against the spec.
	MaxBody int64
	// Route lets the request validator check the request against the spec.
	Route *routers.Route
}

// policies maps "METHOD /path" (spec paths, e.g. "GET /users/{id}").
type policies map[string]*policy

func policyKey(method, path string) string { return method + " " + path }

// loadPolicies reads every operation's policy from the embedded spec. A
// missing or unknown access level is a programming error.
func loadPolicies() (policies, error) {
	spec, err := apigen.GetSpec()
	if err != nil {
		return nil, err
	}
	out := policies{}
	for path, item := range spec.Paths.Map() {
		for method, op := range item.Operations() {
			p := &policy{
				OperationID: op.OperationID,
				Route:       &routers.Route{Spec: spec, Path: path, PathItem: item, Method: method, Operation: op},
			}
			if err := extString(op, "x-panelarr-access", &p.Access); err != nil {
				return nil, err
			}
			switch p.Access {
			case AccessPublic, AccessUser, AccessAdmin:
			default:
				return nil, fmt.Errorf("api: %s %s: x-panelarr-access must be public, user or admin, not %q", method, path, p.Access)
			}
			if err := extBool(op, "x-panelarr-setup", &p.Setup); err != nil {
				return nil, err
			}
			if err := extBool(op, "x-panelarr-session-only", &p.SessionOnly); err != nil {
				return nil, err
			}
			if err := extBool(op, "x-panelarr-read-only", &p.ReadOnly); err != nil {
				return nil, err
			}
			if err := extInt(op, "x-panelarr-max-body", &p.MaxBody); err != nil {
				return nil, err
			}
			if p.Access == AccessPublic && p.SessionOnly {
				return nil, fmt.Errorf("api: %s %s: a public operation can't be session only", method, path)
			}
			out[policyKey(method, path)] = p
		}
	}
	return out, nil
}

func ext(op *openapi3.Operation, name string) (any, bool) {
	v, ok := op.Extensions[name]
	if !ok {
		return nil, false
	}
	if raw, isRaw := v.(json.RawMessage); isRaw {
		var decoded any
		if err := json.Unmarshal(raw, &decoded); err == nil {
			return decoded, true
		}
	}
	return v, true
}

func extString(op *openapi3.Operation, name string, dst *string) error {
	v, ok := ext(op, name)
	if !ok {
		return nil
	}
	s, isString := v.(string)
	if !isString {
		return fmt.Errorf("api: %s: %s must be a string", op.OperationID, name)
	}
	*dst = s
	return nil
}

func extBool(op *openapi3.Operation, name string, dst *bool) error {
	v, ok := ext(op, name)
	if !ok {
		return nil
	}
	b, isBool := v.(bool)
	if !isBool {
		return fmt.Errorf("api: %s: %s must be a boolean", op.OperationID, name)
	}
	*dst = b
	return nil
}

// lookup finds the policy for a matched chi route pattern under BasePath.
func (ps policies) lookup(method, routePattern string) *policy {
	return ps[policyKey(method, strings.TrimPrefix(routePattern, BasePath))]
}

// safeMethod reports whether a method only reads.
func safeMethod(m string) bool {
	return m == http.MethodGet || m == http.MethodHead || m == http.MethodOptions
}

func extInt(op *openapi3.Operation, name string, dst *int64) error {
	v, ok := ext(op, name)
	if !ok {
		return nil
	}
	f, isNumber := v.(float64)
	if !isNumber || f <= 0 || f != float64(int64(f)) {
		return fmt.Errorf("api: %s: %s must be a positive integer", op.OperationID, name)
	}
	*dst = int64(f)
	return nil
}

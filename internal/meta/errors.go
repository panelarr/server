// SPDX-License-Identifier: GPL-3.0-or-later

package meta

import (
	"errors"

	"connectrpc.com/connect"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
)

// ProviderUnavailableError is the metadata service answering that one of
// its provider plugins can't be reached. The service itself is fine.
type ProviderUnavailableError struct {
	Provider string
	Message  string
}

func (e *ProviderUnavailableError) Error() string {
	return "meta: provider " + e.Provider + " is unavailable: " + e.Message
}

// reasonProviderUnavailable is the ErrorInfo reason the metadata service
// may give when a provider's plugin can't be reached.
const reasonProviderUnavailable = "PROVIDER_UNAVAILABLE"

// Unreachable reports whether err means the metadata service couldn't be
// reached at all (no answer: connection refused, DNS, timeout), as
// opposed to the service answering with an error.
func Unreachable(err error) bool {
	var ce *connect.Error
	return errors.As(err, &ce) && !connect.IsWireError(err)
}

// errorInfo returns the ErrorInfo detail of an error the service sent, if
// any.
func errorInfo(err error) (reason string, metadata map[string]string) {
	var ce *connect.Error
	if !errors.As(err, &ce) {
		return "", nil
	}
	for _, d := range ce.Details() {
		v, derr := d.Value()
		if derr != nil {
			continue
		}
		if info, ok := v.(*errdetails.ErrorInfo); ok {
			return info.GetReason(), info.GetMetadata()
		}
	}
	return "", nil
}

// providerError turns the service's answer that a provider can't be
// reached into a ProviderUnavailableError. It recognises the explicit
// reason when the service gives one, and otherwise an Unavailable answer to
// a call about one provider (the service itself answered, so it is up).
func providerError(err error, provider string) error {
	if err == nil || !connect.IsWireError(err) {
		return err
	}
	reason, md := errorInfo(err)
	if reason != reasonProviderUnavailable && connect.CodeOf(err) != connect.CodeUnavailable {
		return err
	}
	if p := md["provider"]; p != "" {
		provider = p
	}
	var ce *connect.Error
	msg := ""
	if errors.As(err, &ce) {
		msg = ce.Message()
	}
	return &ProviderUnavailableError{Provider: provider, Message: msg}
}

// Error is a failed call to the metadata service, so callers that also
// talk to source plugins can tell whose error it is.
type Error struct {
	Op  string
	Err error
}

func (e *Error) Error() string { return "meta: " + e.Op + ": " + e.Err.Error() }
func (e *Error) Unwrap() error { return e.Err }

func wrap(op string, err error) error {
	if err == nil {
		return nil
	}
	var pu *ProviderUnavailableError
	if errors.As(err, &pu) {
		return &Error{Op: op, Err: pu}
	}
	return &Error{Op: op, Err: err}
}

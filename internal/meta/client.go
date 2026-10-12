// SPDX-License-Identifier: GPL-3.0-or-later

// Package meta connects the server to the Panelarr metadata service, which
// the library needs: every work is created from one of its entries. It
// keeps the connection settings, checks that the service is usable and
// wraps the catalogue calls.
package meta

import (
	"context"
	"crypto/tls"
	"net"
	"net/http"
	"strings"
	"time"

	"connectrpc.com/connect"

	"github.com/panelarr/meta/gen/panelarr/meta/v1/metav1connect"

	"github.com/panelarr/server/internal/buildinfo"
)

// Conn is how to reach the metadata service.
type Conn struct {
	// URL is the service's base URL, e.g. "http://meta:8901".
	URL string
	// APIKey is a key with the admin scope.
	APIKey string
}

// Client talks to one metadata service over gRPC, the canonical wire for
// Panelarr services.
type Client struct {
	Meta    metav1connect.MetaServiceClient
	Admin   metav1connect.MetaAdminServiceClient
	Clients metav1connect.MetaClientsServiceClient
	Jobs    metav1connect.MetaJobsServiceClient
	// Integrations takes network profiles pushed by the server.
	Integrations metav1connect.MetaIntegrationsServiceClient
}

// newHTTPClient returns an HTTP/2 client: TLS for https and cleartext
// HTTP/2 (h2c) for http URLs, as gRPC needs.
func newHTTPClient() *http.Client {
	var p http.Protocols
	p.SetHTTP2(true)
	p.SetUnencryptedHTTP2(true)
	return &http.Client{
		Transport: &http.Transport{
			Protocols:           &p,
			DialContext:         (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
			TLSClientConfig:     &tls.Config{MinVersion: tls.VersionTLS12},
			TLSHandshakeTimeout: 5 * time.Second,
			IdleConnTimeout:     90 * time.Second,
		},
	}
}

// NewClient builds a client for conn using hc.
func NewClient(conn Conn, hc *http.Client) *Client {
	base := strings.TrimRight(conn.URL, "/")
	opts := []connect.ClientOption{
		connect.WithGRPC(),
		connect.WithInterceptors(bearer(conn.APIKey)),
	}
	return &Client{
		Meta:         metav1connect.NewMetaServiceClient(hc, base, opts...),
		Admin:        metav1connect.NewMetaAdminServiceClient(hc, base, opts...),
		Clients:      metav1connect.NewMetaClientsServiceClient(hc, base, opts...),
		Jobs:         metav1connect.NewMetaJobsServiceClient(hc, base, opts...),
		Integrations: metav1connect.NewMetaIntegrationsServiceClient(hc, base, opts...),
	}
}

// bearer adds the API key and a user agent to every call.
func bearer(key string) connect.UnaryInterceptorFunc {
	return func(next connect.UnaryFunc) connect.UnaryFunc {
		return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
			if key != "" {
				req.Header().Set("Authorization", "Bearer "+key)
			}
			req.Header().Set("User-Agent", "panelarr-server/"+buildinfo.Version)
			return next(ctx, req)
		}
	}
}

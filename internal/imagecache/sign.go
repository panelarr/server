// SPDX-License-Identifier: GPL-3.0-or-later

package imagecache

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"

	"github.com/jackc/pgx/v5"

	"github.com/panelarr/server/internal/store"
	"github.com/panelarr/server/internal/store/db"
)

// Signer signs image references the server hands out (e.g. a source's
// cover reference), so the image endpoint only fetches what the server
// itself listed, never an arbitrary URL a caller makes up.
type Signer struct{ key []byte }

// NewSigner returns a signer with key.
func NewSigner(key []byte) *Signer { return &Signer{key: key} }

const settingSigningKey = "images.signing_key"

// LoadSigner reads the instance's signing key, creating it on first use.
func LoadSigner(ctx context.Context, st *store.Store) (*Signer, error) {
	raw, err := st.GetSetting(ctx, settingSigningKey)
	if err == nil {
		var enc string
		if err := json.Unmarshal(raw, &enc); err != nil {
			return nil, err
		}
		key, err := base64.StdEncoding.DecodeString(enc)
		if err != nil {
			return nil, err
		}
		return NewSigner(key), nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}
	key := make([]byte, 32)
	_, _ = rand.Read(key)
	val, _ := json.Marshal(base64.StdEncoding.EncodeToString(key))
	if err := st.PutSetting(ctx, db.PutSettingParams{Key: settingSigningKey, Value: val}); err != nil {
		return nil, err
	}
	return NewSigner(key), nil
}

// Sign returns a signature over parts.
func (s *Signer) Sign(parts ...string) string {
	m := hmac.New(sha256.New, s.key)
	for _, p := range parts {
		_, _ = m.Write([]byte(p))
		_, _ = m.Write([]byte{0})
	}
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil)[:18])
}

// Verify reports whether sig signs parts.
func (s *Signer) Verify(sig string, parts ...string) bool {
	return hmac.Equal([]byte(sig), []byte(s.Sign(parts...)))
}

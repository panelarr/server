// SPDX-License-Identifier: GPL-3.0-or-later

package reader

import (
	"errors"
	"testing"
)

func TestCheckImage(t *testing.T) {
	for in, want := range map[string]string{"image/png": "image/png", "image/jpg": "image/jpeg", "image/webp; q=1": "image/webp"} {
		if got, err := checkImage(in); err != nil || got != want {
			t.Errorf("%q: %q %v", in, got, err)
		}
	}
	for _, bad := range []string{"image/svg+xml", "text/html", ""} {
		if _, err := checkImage(bad); !errors.Is(err, ErrNotImage) {
			t.Errorf("%q accepted", bad)
		}
	}
}

// SPDX-License-Identifier: GPL-3.0-or-later

package library

import (
	"errors"
	"testing"
)

func TestViewDefinitionContentRatings(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ in, want string }{
		{`{"content_ratings":["safe","suggestive","unknown"]}`, `{"content_ratings":["safe","suggestive","unknown"]}`},
		{`{"content_ratings":["erotic","pornographic"]}`, `{"content_ratings":["erotic","pornographic"]}`},
		{`{"content_ratings":["unknown","pornographic","erotic","suggestive","safe"]}`, `{"content_ratings":["unknown","pornographic","erotic","suggestive","safe"]}`},
		// An empty list means any rating, and is stored as absent.
		{`{"content_ratings":[],"sort":"title"}`, `{"sort":"title"}`},
		{`{"content_ratings":null}`, `{}`},
		// Definitions saved before the filter existed still validate.
		{`{"types":["manga"],"sort":"added"}`, `{"types":["manga"],"sort":"added"}`},
	} {
		got, err := cleanViewDefinition([]byte(tc.in))
		if err != nil || string(got) != tc.want {
			t.Errorf("%s: got %s, %v; want %s", tc.in, got, err, tc.want)
		}
	}

	for _, in := range []string{
		`{"content_ratings":["nsfw"]}`,
		`{"content_ratings":[""]}`,
		`{"content_ratings":["Safe"]}`,
		`{"content_ratings":["safe","safe"]}`,
		`{"content_ratings":["safe","suggestive","erotic","pornographic","unknown","safe"]}`,
	} {
		_, err := cleanViewDefinition([]byte(in))
		var ve *ViewError
		if !errors.As(err, &ve) || ve.Field != "definition.content_ratings" {
			t.Errorf("%s: %v, want a definition.content_ratings error", in, err)
		}
	}
	// Not an array: refused by the strict decode.
	for _, in := range []string{`{"content_ratings":"safe"}`, `{"content_ratings":[1]}`, `{"content_ratings":{}}`} {
		var ve *ViewError
		if _, err := cleanViewDefinition([]byte(in)); !errors.As(err, &ve) {
			t.Errorf("%s: %v, want a ViewError", in, err)
		}
	}
}

// SPDX-License-Identifier: GPL-3.0-or-later

package users

import (
	"strings"
	"testing"

	"github.com/panelarr/server/internal/store/db"
)

func TestGrantsPatch(t *testing.T) {
	yes, ceiling := true, "erotic"
	sources := []string{"b:1", "a:1", "b:1"}
	g := (&GrantsPatch{MayDownload: &yes, ContentRatingCeiling: &ceiling, AllowedSources: &sources}).Apply(DefaultGrants())
	if !g.MayAdd || !g.MayDownload || g.MayManageMatches || g.ContentRatingCeiling != "erotic" || strings.Join(g.AllowedSources, ",") != "a:1,b:1" {
		t.Fatalf("got %+v", g)
	}
	var nilPatch *GrantsPatch
	if got := nilPatch.Apply(DefaultGrants()); got.ContentRatingCeiling != "suggestive" {
		t.Fatalf("nil patch changed grants: %+v", got)
	}
	bad := "extreme"
	if err := (&GrantsPatch{ContentRatingCeiling: &bad}).Apply(DefaultGrants()).Validate(); err == nil {
		t.Fatal("unknown rating accepted")
	}
}

func TestEffective(t *testing.T) {
	u := db.User{Role: "user", ContentRatingCeiling: "safe", AllowedSources: nil}
	if g := Effective(u); g.MayDownload || g.ContentRatingCeiling != "safe" || g.AllowedSources == nil {
		t.Fatalf("user %+v", g)
	}
	u.Role = "admin"
	if g := Effective(u); !g.MayDownload || !g.MayManageMatches || g.ContentRatingCeiling != "pornographic" || !g.AllSources {
		t.Fatalf("admin %+v", g)
	}
}

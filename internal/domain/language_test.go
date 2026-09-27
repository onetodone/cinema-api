package domain

import (
	"strings"
	"testing"
)

func TestCheckLanguage(t *testing.T) {
	t.Parallel()

	for _, code := range []string{"eng", "tha", "rus", "ukr", "jpn", "yue", "cmn", "zho", "fra", "deu"} {
		if err := checkLanguage(code); err != nil {
			t.Errorf("checkLanguage(%q) = %v, want nil", code, err)
		}
	}

	for _, tt := range []struct {
		code, want string
	}{
		{code: "", want: "3-letter lowercase"},
		{code: "en", want: "3-letter lowercase"},  // ISO 639-1
		{code: "ENG", want: "3-letter lowercase"}, // the service lower-cases; the domain takes the canonical form only
		{code: "en-", want: "3-letter lowercase"},
		{code: "engl", want: "3-letter lowercase"},
		{code: "ënd", want: "3-letter lowercase"},
		{code: "xyz", want: "known language"},                    // well-formed, not a language
		{code: "und", want: "known language"},                    // "undetermined"
		{code: "fre", want: "current code of the language: fra"}, // ISO 639-2/B
		{code: "ger", want: "current code of the language: deu"},
		{code: "chi", want: "current code of the language: zho"},
	} {
		err := checkLanguage(tt.code)
		if err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("checkLanguage(%q) = %v, want an error containing %q", tt.code, err, tt.want)
		}
	}
}

func TestGenres(t *testing.T) {
	t.Parallel()

	all := Genres()
	if len(all) != 18 {
		t.Fatalf("%d genres, want 18", len(all))
	}
	for i, g := range all {
		if !g.Valid() {
			t.Errorf("%q is listed but not valid", g)
		}
		if i > 0 && all[i-1] >= g {
			t.Errorf("genres are not in alphabetical order at %q", g)
		}
	}
	all[0] = "noir" // the list is a copy
	if Genre("noir").Valid() || Genres()[0] != GenreAction {
		t.Error("changing the returned list changed the vocabulary")
	}
}

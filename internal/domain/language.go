package domain

import (
	"errors"
	"fmt"

	"golang.org/x/text/language"
)

// LanguageVersion is the language a showtime is screened in: the language of its soundtrack, and of its
// subtitles if it has any. Both are ISO 639-3 codes such as eng or tha, which clients show upper-cased, as in
// "ENG | SUB: THA".
type LanguageVersion struct {
	Audio     string
	Subtitles string // empty when the showtime has no subtitles
}

// check reports an invalid audio or subtitle language under the request fields audio_language and
// subtitle_language. The audio language is required.
func (lv LanguageVersion) check(v *Violations) {
	if lv.Audio == "" {
		v.Add("audio_language", "is required")
	} else {
		v.Check("audio_language", checkLanguage(lv.Audio))
	}
	if lv.Subtitles != "" {
		v.Check("subtitle_language", checkLanguage(lv.Subtitles))
	}
}

// checkLanguage accepts the ISO 639-3 code of a known language in its canonical lowercase form, so that every
// language has exactly one spelling: eng, not en or ENG; fra, not the bibliographic fre.
func checkLanguage(code string) error {
	if !isLowerAlpha3(code) {
		return errors.New("must be a 3-letter lowercase ISO 639-3 language code, such as eng")
	}
	base, err := language.ParseBase(code)
	if err != nil || base.ISO3() != code || code == "und" {
		return errors.New("must be the ISO 639-3 code of a known language, such as eng")
	}
	// Legacy canonicalization maps the bibliographic and deprecated codes, such as fre or ger, to the ones in use.
	if tag, err := language.Legacy.Parse(code); err == nil {
		if canon, _ := tag.Base(); canon.ISO3() != code {
			return fmt.Errorf("must be the current code of the language: %s", canon.ISO3())
		}
	}
	return nil
}

func isLowerAlpha3(s string) bool {
	if len(s) != 3 {
		return false
	}
	for _, c := range []byte(s) {
		if c < 'a' || c > 'z' {
			return false
		}
	}
	return true
}

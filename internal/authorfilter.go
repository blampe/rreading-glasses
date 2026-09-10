package internal

import (
	"context"
	"os"
	"regexp"
	"strings"
	"unicode"
)

// This file adds opt-out filtering of an author's work list so that foreign
// editions, split "parts", and multi-book box sets / bundles / omnibuses don't
// pollute the bibliography Readarr/bookshelf sees. It is applied in
// denormalizeWorks, which is the single point every author<->work relationship
// flows through.
//
// Toggle off with RG_FILTER_WORKS=false (or 0 / no / off).

var (
	// Titles that look like a single book split across volumes for printing.
	// Must be an explicit "N of M" / "N/M" — a bare "Part 1" is usually book 1
	// of a series (e.g. "Chainfire Trilogy, Part 1"), not a split edition.
	_partPatterns = []*regexp.Regexp{
		regexp.MustCompile(`(?i)\b(?:part|pt\.?|vol(?:ume)?\.?|teil|deel|parte|partie)\s+\d+\s+of\s+\d+\b`),
		regexp.MustCompile(`(?i)\b(?:part|vol(?:ume)?)\s+\d+\s*/\s*\d+\b`),
		regexp.MustCompile(`(?i)\(\s*\d+\s+of\s+\d+\s*\)`),
	}

	// Distinctly French/Italian/Catalan orthography that HC often fails to tag
	// with a language. Kept narrow to avoid nuking English titles and proper
	// nouns (e.g. Goodkind's "D'Hara" — capital letter after the apostrophe).
	_foreignWordPatterns = []*regexp.Regexp{
		regexp.MustCompile(`(?i)\btome\s+\d+`), // French "volume N"
		// Distinctly French elided articles l' j' qu' at a word boundary —
		// very rare in English, and avoids "D'Hara" / "O'Brien" / "Assassin's".
		regexp.MustCompile(`(?i)(?:^|\s)(?:l|j|qu)['’]\p{L}`),
		// German: article/conjunction + word ("der Wahrheit", "das Schwert", "und ...").
		regexp.MustCompile(`(?i)\b(?:der|das|des|dem|und)\s+\p{L}{3,}`),
		// Dutch: "het ...", "van de ...".
		regexp.MustCompile(`(?i)\bhet\s+\p{L}{3,}`),
		regexp.MustCompile(`(?i)\bvan\s+(?:de|het|den)\s+\p{L}`),
		// Spanish/Portuguese: "de los", "de las", "del ...".
		regexp.MustCompile(`(?i)\bde\s+(?:los|las)\b`),
		regexp.MustCompile(`(?i)\bdel\s+\p{L}{3,}`),
		// Italian: "della/dello/degli ...", "vol\.".
		regexp.MustCompile(`(?i)\b(?:della|dello|degli|delle)\s+\p{L}`),
		regexp.MustCompile(`(?i),\s*vol\.\s*[0-9ivxl]+\s*$`),
	}

	// Titles that look like a bundle of multiple books rather than one work.
	_setPatterns = []*regexp.Regexp{
		regexp.MustCompile(`(?i)\bbox(?:ed)?\s?set\b`),
		regexp.MustCompile(`(?i)\bslipcase\b`),
		regexp.MustCompile(`(?i)\bomnibus\b`),
		regexp.MustCompile(`(?i)\bbundle\b`),
		regexp.MustCompile(`(?i)\b\d+[-\s]?book\s+(?:set|collection|bundle|box|pack|series)\b`),
		regexp.MustCompile(`(?i)\bbooks?\s+\d+\s*(?:[-–—]|to|thru|through|&|and)\s*\d+\b`),
		regexp.MustCompile(`(?i)\b\d+\s+books?\s+in\s+(?:one|1)\b`),
		regexp.MustCompile(`(?i)\bset\s+of\s+\d+\s+books?\b`),
		regexp.MustCompile(`(?i)\bthe\s+complete\s+(?:series|saga|collection|novels|quartet|trilogy|duology)\b`),
		regexp.MustCompile(`(?i)\bcollection\s*:?\s*books?\s+\d`),
		regexp.MustCompile(`(?i)\b\d+\s+book\s+set\s+collection\b`),
		regexp.MustCompile(`(?i)\b\d+\s*[-–—]\s*\d+\s+set\b`),           // "Throne of Glass 1-3 set"
		regexp.MustCompile(`(?i):\s*\d+\s*[-–—]\s*\d+\s*$`),             // "Throne of Glass: 1-3"
		regexp.MustCompile(`(?i)\bset:\s+a\s+\d+\s+book\b`),             // "Set: A 5 Book Bundle"
		regexp.MustCompile(`(?i)\ba\s+\d+\s+book\s+(?:bundle|set|collection)\b`),
		regexp.MustCompile(`(?i)^[^,:]+,\s+[^,:]+,\s+[^,:]+\s+and\s+[^,:]+$`), // 3+ book titles joined "A, B and C"
		regexp.MustCompile(`(?i)\bset:\s*\(`),                                 // "A Sword of Truth Set: (Book, Book, ...)"
		regexp.MustCompile(`(?i)\blot of \d+\s+(?:novels?|books?)\b`),          // "Lot of 11 Novels"
		regexp.MustCompile(`(?i)\bset:\s+\p{L}+,\s+\p{L}`),                     // "Set: Chainfire, Phantom, Confessor"
	}

	// ISO-639 codes / bare names we treat as "English or unknown".
	_englishLangs = map[string]bool{"": true, "eng": true, "en": true, "english": true}
)

func filterWorksEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("RG_FILTER_WORKS"))) {
	case "false", "0", "no", "off":
		return false
	default:
		return true
	}
}

// excludeWorkFromAuthor reports whether a work should be kept out of an author's
// bibliography, plus a short reason for logging.
func excludeWorkFromAuthor(ctx context.Context, w workResource) (bool, string) {
	if !filterWorksEnabled() {
		return false, ""
	}

	// Check both the display title and the title+subtitle.
	titles := []string{strings.TrimSpace(w.Title), strings.TrimSpace(w.FullTitle)}

	// 1. Explicit non-English edition language (HC populates this on the edition).
	if len(w.Books) > 0 {
		lang := strings.ToLower(strings.TrimSpace(w.Books[0].Language))
		if !_englishLangs[lang] {
			return true, "non-english language: " + lang
		}
	}

	for _, title := range titles {
		if title == "" {
			continue
		}

		// 2. Predominantly non-Latin script (Hebrew, Cyrillic, CJK, Greek, ...).
		if isMostlyNonLatin(title) {
			return true, "non-latin-script title"
		}

		// 2b. Latin-script but heavy with diacritics rare in English (Lithuanian,
		// Polish, Portuguese, Czech/Slovak, German, etc.) — HC often leaves
		// Language blank on these, so catch them by orthography.
		if hasForeignDiacritics(title) {
			return true, "foreign diacritics in title"
		}

		// 2c. Distinctly French/Italian function words / elisions.
		for _, re := range _foreignWordPatterns {
			if re.MatchString(title) {
				return true, "foreign-language title"
			}
		}

		// 3. Split "parts" of a single book.
		for _, re := range _partPatterns {
			if re.MatchString(title) {
				return true, "looks like a split part"
			}
		}

		// 4. Multi-book sets / bundles / omnibuses.
		for _, re := range _setPatterns {
			if re.MatchString(title) {
				return true, "looks like a box set / bundle"
			}
		}
	}

	return false, ""
}

// Diacritics/letters that essentially never appear in English titles but are
// common in Lithuanian, Polish, Portuguese, Czech, Slovak, Turkish, German, etc.
// Deliberately excludes é ë à è ê â î ô û ï — those show up in English loanwords
// (café, naïve, Zoë, ...).
const _foreignLetters = "ąćęłńóśźżãõçğışåæøðþĳœ" +
	"āēīūčšžėųįňřůťďĺľŕ" +
	"äöüß" +
	"íúÍÚ" +
	"ıİ"

// hasForeignDiacritics flags a title carrying any distinctly non-English letter.
func hasForeignDiacritics(s string) bool {
	for _, r := range strings.ToLower(s) {
		if strings.ContainsRune(_foreignLetters, r) {
			return true
		}
	}
	return false
}

// isMostlyNonLatin returns true when, ignoring digits/spaces/punctuation, most
// of the letters in s are outside the Latin script.
func isMostlyNonLatin(s string) bool {
	var latin, other int
	for _, r := range s {
		if !unicode.IsLetter(r) {
			continue
		}
		if unicode.Is(unicode.Latin, r) {
			latin++
		} else {
			other++
		}
	}
	if latin+other == 0 {
		return false
	}
	return other*100/(latin+other) >= 40
}

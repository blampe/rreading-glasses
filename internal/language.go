package internal

var _codes = map[string]string{
	"English":          "eng",
	"French":           "fra",
	"Spanish":          "spa",
	"German":           "deu",
	"Italian":          "ita",
	"Danish":           "dan",
	"Dutch":            "nld",
	"Japanese":         "jpn",
	"Icelandic":        "isl",
	"Chinese":          "zho",
	"Russian":          "rus",
	"Polish":           "pol",
	"Vietnamese":       "vie",
	"Swedish":          "swe",
	"Norwegian":        "nor",
	"Norwegian Bokmal": "nob",
	"Finnish":          "fin",
	"Turkish":          "tur",
	"Portuguese":       "por",
	"Greek":            "ell",
	"Korean":           "kor",
	"Hungarian":        "hun",
	"Hebrew":           "heb",
	"Czech":            "ces",
	"Hindi":            "hin",
	"Thai":             "tha",
	"Bulgarian":        "bul",
	"Romanian":         "ron",
	"Arabic":           "ara",
}

func iso639_3(name string) (iso string) {
	iso, ok := _codes[name]
	if ok {
		return iso
	}
	return name
}

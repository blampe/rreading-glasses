package main

// TODO: These could be generated from the OpenAPI spec.
// https://github.com/Readarr/Readarr/blob/develop/src/Readarr.Api.V1/openapi.json

type bulkBookResource struct {
	Works   []workResource   `json:"Works"`
	Series  []seriesResource `json:"Series"`
	Authors []authorResource `json:"Authors"`
}

type workResource struct {
	ForeignID    int64    `json:"ForeignId"`
	Title        string   `json:"Title"`
	URL          string   `json:"Url"`
	ReleaseDate  string   `json:"ReleaseDate,omitempty"`
	Genres       []string `json:"Genres"`
	RelatedWorks []int    `json:"RelatedWorks"` // ForeignId

	Books   []bookResource   `json:"Books"`
	Series  []seriesResource `json:"Series"`
	Authors []authorResource `json:"Authors"`

	// New fields
	KCA        string `json:"KCA"`
	BestBookID int64  `json:"BestBookId"`
}

type authorResource struct {
	ForeignID     int64   `json:"ForeignId"`
	Name          string  `json:"Name"`
	Description   string  `json:"Description"`
	ImageURL      string  `json:"ImageUrl"`
	URL           string  `json:"Url"`
	RatingCount   int64   `json:"RatingCount"`
	AverageRating float32 `json:"AverageRating"`

	// Relations.
	Works  []workResource   `json:"Works"`
	Series []seriesResource `json:"Series"`

	// New fields.
	KCA string `json:"KCA"`
}

type bookResource struct {
	ForeignID          int64   `json:"ForeignId"`
	Asin               string  `json:"Asin"`
	Description        string  `json:"Description"`
	Isbn13             string  `json:"Isbn13,omitempty"`
	Title              string  `json:"Title"`
	Language           string  `json:"Language"`
	Format             string  `json:"Format"`
	EditionInformation string  `json:"EditionInformation"`
	Publisher          string  `json:"Publisher"`
	ImageURL           string  `json:"ImageUrl"`
	IsEbook            bool    `json:"IsEbook"`
	NumPages           int64   `json:"NumPages"`
	RatingCount        int64   `json:"RatingCount"`
	AverageRating      float64 `json:"AverageRating"`
	URL                string  `json:"Url"`
	ReleaseDate        string  `json:"ReleaseDate,omitempty"`

	Contributors []contributorResource `json:"Contributors"`

	// New fields
	KCA       string `json:"KCA"`
	RatingSum int64  `json:"RatingSum"`
}

type seriesResource struct {
	ForeignID   int64  `json:"ForeignId"`
	Title       string `json:"Title"`
	Description string `json:"Description"`

	LinkItems []seriesWorkLinkResource `json:"LinkItems"`

	// New fields
	KCA string `json:"KCA"`
}

type seriesWorkLinkResource struct {
	ForeignWorkID    int64  `json:"ForeignWorkId"`
	PositionInSeries string `json:"PositionInSeries"`
	SeriesPosition   int    `json:"SeriesPosition"`
	Primary          bool   `json:"Primary"`
}

type contributorResource struct {
	ForeignID int64  `json:"ForeignId"`
	Role      string `json:"Role"`
}

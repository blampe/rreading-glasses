package hardcover

import "fmt"

// AsContributions maps a slice of Contributions fragments into... a slice of
// Contributions fragments. I must not be thinking clearly because this seems
// like it should be a more straightforward interface...
func AsContributions(c any) []Contributions {
	result := []Contributions{}

	switch s := c.(type) {
	case []DefaultEditionsContributions:
		for _, cc := range s {
			result = append(result, cc.Contributions)
		}
	case []DefaultEditionsDefault_audio_editionEditionsContributions:
		for _, cc := range s {
			result = append(result, cc.Contributions)
		}
	case []DefaultEditionsDefault_cover_editionEditionsContributions:
		for _, cc := range s {
			result = append(result, cc.Contributions)
		}
	case []DefaultEditionsDefault_ebook_editionEditionsContributions:
		for _, cc := range s {
			result = append(result, cc.Contributions)
		}
	case []DefaultEditionsDefault_physical_editionEditionsContributions:
		for _, cc := range s {
			result = append(result, cc.Contributions)
		}
	case []GetAuthorEditionsAuthors_by_pkAuthorsContributions:
		for _, cc := range s {
			result = append(result, cc.Contributions)
		}
	default:
		panic(fmt.Errorf("unrecognized contribution type %T", c))
	}
	return result
}

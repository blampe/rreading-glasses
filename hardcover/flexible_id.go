package hardcover

import (
	"bytes"
	"encoding/json"
	"strconv"
)

// FlexibleID is a string that unmarshals from either a JSON string or number.
// Hardcover's `search.ids` field has historically been [String] but the API
// now returns numbers, so we accept both. See upstream issue #548.
type FlexibleID string

func (f *FlexibleID) UnmarshalJSON(data []byte) error {
	data = bytes.TrimSpace(data)
	if len(data) > 0 && data[0] == '"' {
		var s string
		if err := json.Unmarshal(data, &s); err != nil {
			return err
		}
		*f = FlexibleID(s)
		return nil
	}
	var n json.Number
	if err := json.Unmarshal(data, &n); err != nil {
		return err
	}
	*f = FlexibleID(n.String())
	return nil
}

func (f FlexibleID) String() string { return string(f) }

// Int64 parses the ID as int64.
func (f FlexibleID) Int64() (int64, error) {
	return strconv.ParseInt(string(f), 10, 64)
}

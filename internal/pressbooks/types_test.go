package pressbooks

import (
	"encoding/json"
	"testing"
)

// One network sends copyrightYear as a string, another as a number. A plain
// string field makes every book on the second network fail to decode.
func TestFlexStringAcceptsStringsAndNumbers(t *testing.T) {
	var got struct {
		Year flexString `json:"copyrightYear"`
	}
	for input, want := range map[string]string{
		`{"copyrightYear":"2017"}`: "2017",
		`{"copyrightYear":2017}`:   "2017",
		`{"copyrightYear":null}`:   "",
		`{"copyrightYear":""}`:     "",
		`{}`:                       "",
	} {
		got.Year = "unset"
		if err := json.Unmarshal([]byte(input), &got); err != nil {
			t.Fatalf("decode %s: %v", input, err)
		}
		if input == `{}` {
			want = "unset"
		}
		if got.Year.String() != want {
			t.Errorf("decode %s = %q, want %q", input, got.Year, want)
		}
	}
}

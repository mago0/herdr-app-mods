package created

import (
	"encoding/json"
	"os"
	"slices"
	"testing"
)

type t3Case struct {
	Name   string `json:"name"`
	Input  string `json:"input"`
	Output string `json:"output"`
	Linked []int  `json:"linked"`
}

// The cases come from T3 Code's PullRequestSyncReactor tests, so both ports agree on what a create command is.
func TestByT3Cases(t *testing.T) {
	raw, err := os.ReadFile("testdata/t3_cases.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []t3Case
	if err := json.Unmarshal(raw, &cases); err != nil {
		t.Fatal(err)
	}
	if len(cases) != 21 {
		t.Fatalf("want 21 cases, got %d", len(cases))
	}
	for _, c := range cases {
		t.Run(c.Name, func(t *testing.T) {
			var got []int
			for _, r := range By(c.Input, c.Output) {
				got = append(got, r.Number)
			}
			if !slices.Equal(got, c.Linked) && !(len(got) == 0 && len(c.Linked) == 0) {
				t.Errorf("By(%q, %q) = %v, want %v", c.Input, c.Output, got, c.Linked)
			}
		})
	}
}

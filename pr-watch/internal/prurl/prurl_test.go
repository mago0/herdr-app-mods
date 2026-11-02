package prurl

import "testing"

func TestParseVariants(t *testing.T) {
	want := "https://github.com/flocasts/iam/pull/412"
	for _, in := range []string{
		"https://github.com/FloCasts/IAM/pull/412",
		"http://github.com/flocasts/iam/pull/412/files",
		"github.com/flocasts/iam/pull/412/",
	} {
		r, err := Parse(in)
		if err != nil {
			t.Fatalf("Parse(%q): %v", in, err)
		}
		if r.URL() != want {
			t.Errorf("Parse(%q).URL() = %q, want %q", in, r.URL(), want)
		}
		if r.Repo() != "flocasts/iam" || r.Number != 412 {
			t.Errorf("Parse(%q) repo/number = %q/%d", in, r.Repo(), r.Number)
		}
	}
	for _, in := range []string{"https://github.com/flocasts/iam/issues/412", "not a url"} {
		if _, err := Parse(in); err == nil {
			t.Errorf("Parse(%q): want error", in)
		}
	}
}

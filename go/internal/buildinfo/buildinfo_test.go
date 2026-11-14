package buildinfo

import "testing"

func TestString(t *testing.T) {
	defer func(v, c string) { Version, Commit = v, c }(Version, Commit)
	Version, Commit = "1.2.3", "none"
	if String() != "1.2.3" {
		t.Fatalf("got %q", String())
	}
	Commit = ""
	if String() != "1.2.3" {
		t.Fatalf("got %q", String())
	}
	Commit = "abc"
	if String() != "1.2.3 (abc)" {
		t.Fatalf("got %q", String())
	}
}

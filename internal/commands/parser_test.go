package commands

import (
	"reflect"
	"testing"
)

func TestParseLineQuotesWithoutShellExpansion(t *testing.T) {
	got, err := ParseLine(`/apply "plan file.md" 'literal $HOME' a\\ b`)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"/apply", "plan file.md", "literal $HOME", "a\\", "b"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ParseLine=%#v, want %#v", got, want)
	}
}

func TestParseLineRejectsIncompleteQuotes(t *testing.T) {
	for _, input := range []string{`/help "id`, `/help trailing\`} {
		if _, err := ParseLine(input); err == nil {
			t.Fatalf("ParseLine(%q) accepted malformed input", input)
		}
	}
}

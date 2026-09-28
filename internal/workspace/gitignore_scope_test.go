package workspace

import "testing"

func TestIgnoreRulesUseTheirOwnDirectory(t *testing.T) {
	for _, tc := range []struct {
		pattern, path   string
		directory, want bool
	}{
		{"/secret", "secret", false, false},
		{"/secret", "sub/secret", false, true},
		{"/secret", "sub/nested/secret", false, false},
		{"secret", "sub/nested/secret", false, true},
		{"build/*.o", "sub/build/main.o", false, true},
		{"build/*.o", "sub/nested/build/main.o", false, false},
		{"cache/", "sub/cache/data", false, true},
	} {
		rule := parseIgnoreLine(tc.pattern)
		rule.baseDir = "sub"
		if got := matchRule(rule, tc.path, tc.directory); got != tc.want {
			t.Errorf("%s %s: %v", tc.pattern, tc.path, got)
		}
	}
}

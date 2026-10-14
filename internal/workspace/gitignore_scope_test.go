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

func TestTrailingDoubleStarAllowsReincludedLeaf(t *testing.T) {
	m := NewMatcherSafe()
	m.AddGitignore([]byte("dir/**\n!dir/keep.txt\n"), "")
	for _, test := range []struct {
		path         string
		dir, ignored bool
	}{{"dir", true, false}, {"dir/keep.txt", false, false}, {"dir/drop.txt", false, true}} {
		ignored, _ := m.Match(test.path, test.dir)
		if ignored != test.ignored {
			t.Errorf("%s ignored=%v, want %v", test.path, ignored, test.ignored)
		}
	}
}

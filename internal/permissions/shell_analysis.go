package permissions

import "ccdp/internal/shellsyntax"

type shellSegment struct {
	text  string
	words []string
}

func analyzeShell(command string) ([]shellSegment, bool) {
	parsed, ok := shellsyntax.Analyze(command)
	segments := make([]shellSegment, len(parsed))
	for i, segment := range parsed {
		segments[i] = shellSegment{segment.Text, segment.Words}
	}
	return segments, ok
}

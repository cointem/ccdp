package permissions

import (
	"path/filepath"
	"strings"

	"ccdp/internal/shellsyntax"
)

func dangerousShellCommand(command string, depth int) string {
	segments, static := analyzeShell(command)
	if !static || depth >= 8 {
		return dangerousPatterns(command)
	}
	for _, segment := range segments {
		words, supported := shellsyntax.Executable(segment.words)
		if !supported {
			if reason := dangerousPatterns(command); reason != "" {
				return reason
			}
			continue
		}
		if len(words) == 0 {
			continue
		}
		if nested, shell := shellsyntax.ShellProgram(words); shell {
			if reason := dangerousShellCommand(nested, depth+1); reason != "" {
				return reason
			}
		}
		name := filepath.Base(words[0])
		switch name {
		case "shutdown", "reboot", "poweroff", "halt":
			return "system power control"
		case "mkfs", "fdisk", "parted":
			return "filesystem formatting or partition editing"
		case "rm", "dd", "chmod", "git":
			// These rules describe dangerous operands, so retain their existing
			// matching after confirming the actual executable position.
			normalized := append([]string{name}, words[1:]...)
			commandText := strings.Join(normalized, " ")
			for _, pattern := range denyPatterns {
				if strings.HasPrefix(pattern.re.String(), name+`\s`) {
					if match := pattern.re.FindStringIndex(commandText); match != nil && match[0] == 0 {
						return pattern.desc
					}
				}
			}
		case "curl", "wget":
			for _, pattern := range denyPatterns {
				if strings.Contains(pattern.desc, "remote execution") && pattern.re.MatchString(command) {
					return pattern.desc
				}
			}
		}
		if strings.HasPrefix(name, "mkfs.") {
			return "mkfs (filesystem formatting)"
		}
	}
	return ""
}

func dangerousPatterns(command string) string {
	for _, pattern := range denyPatterns {
		if pattern.re.MatchString(command) {
			return pattern.desc
		}
	}
	return ""
}

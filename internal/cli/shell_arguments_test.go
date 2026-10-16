package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"unicode"
)

func TestTerminalShellArgumentPreservesLiteralBytes(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "injected")
	allBytes := make([]byte, 255)
	for i := range allBytes {
		allBytes[i] = byte(i + 1) // NUL cannot occur in a filesystem path or argv.
	}
	tests := []struct {
		name  string
		value string
	}{
		{"empty", ""},
		{"ordinary quote", "space and 'quote' and \\slash"},
		{"ordinary unicode", "한글 경로"},
		{"newline and substitutions", "workspace\n$(touch '" + marker + "')`touch '" + marker + "'`\n\n"},
		{"ansi", "workspace\x1b[31m"},
		{"unicode format", "workspace\u202evalue\u2028"},
		{"invalid utf8", "workspace\xff\xfe"},
		{"every nonzero byte", string(allBytes)},
	}
	for _, shell := range []string{"bash", "zsh"} {
		t.Run(shell, func(t *testing.T) {
			binary, err := exec.LookPath(shell)
			if err != nil {
				t.Skipf("%s is unavailable: %v", shell, err)
			}
			for _, tc := range tests {
				t.Run(tc.name, func(t *testing.T) {
					argument := terminalShellArgument(tc.value)
					for _, r := range argument {
						if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || r == '\u2028' || r == '\u2029' {
							t.Fatalf("argument contains terminal control character: %q", argument)
						}
					}
					out, err := exec.Command(binary, "-c", "printf '%s' "+argument).CombinedOutput()
					if err != nil {
						t.Fatalf("shell rejected %q: %v: %s", argument, err, out)
					}
					if string(out) != tc.value {
						t.Fatalf("argument %q produced %q, want %q", argument, out, tc.value)
					}
					if _, err := os.Stat(marker); !os.IsNotExist(err) {
						t.Fatalf("shell executed a literal substitution: %v", err)
					}
				})
			}
		})
	}
}

func TestTerminalShellArgumentNormalValuesUsePortableQuoting(t *testing.T) {
	for _, value := range []string{"", "plain/path", "space and 'quote'", "$(printf injected)`printf injected`", "한글 경로"} {
		argument := terminalShellArgument(value)
		if strings.HasPrefix(argument, "$'") {
			t.Fatalf("ordinary value requires ANSI-C quoting: %q", argument)
		}
		out, err := exec.Command("sh", "-c", "printf '%s' "+argument).CombinedOutput()
		if err != nil || string(out) != value {
			t.Fatalf("portable argument %q produced %q, want %q: %v", argument, out, value, err)
		}
	}
}

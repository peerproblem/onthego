package cli

import (
	"fmt"
	"strings"

	"github.com/peerproblem/onthego/internal/agentsessions"
	"github.com/peerproblem/onthego/internal/otgerror"

	"github.com/peerproblem/onthego/internal/capture"
	"github.com/peerproblem/onthego/internal/syncer"
)

func renderTransfer(action string, r syncer.Result) string {
	msg := terminalValue(action) + " → " + terminalValue(r.Root)
	if r.Sessions.Status == "recovery_needed" {
		msg += " (native session recovery needed)"
	}
	if r.Backup != "" {
		msg += "\nPrevious workspace preserved in Git backup " + terminalValue(r.Backup) + "; use onthego undo to restore it"
	}
	if r.SessionsRetried {
		msg += "\nRetried native session import; workspace files were preserved."
	}
	if r.Sessions.Status == "recovery_needed" {
		msg += "\nNative session recovery pending; run onthego sessions recover."
	}
	seenWarnings := map[string]bool{}
	for _, warning := range append(append([]string{}, r.Warnings...), r.Sessions.Warnings...) {
		if !seenWarnings[warning] {
			msg += "\nWarning: " + terminalValue(warning)
			seenWarnings[warning] = true
		}
	}
	root := terminalShellArgument(r.Root)
	msg += "\nContinue in the receiving workspace: cd " + root
	msg += fmt.Sprintf("\nAgent histories: %d imported, %d updated, %d unchanged.", r.Sessions.Imported, r.Sessions.Updated, r.Sessions.Skipped)
	for _, file := range r.Manifest.Files {
		if file.Secret {
			msg += "\nManaged secret file: " + terminalValue(file.RelativePath) + " (included in subsequent handoffs)"
		}
	}
	seen := map[string]bool{}
	for _, session := range r.Sessions.Sessions {
		if session.ID == "" || seen[session.Agent+session.ID] {
			continue
		}
		seen[session.Agent+session.ID] = true
		id := terminalShellArgument(session.ID)
		if session.Agent == "codex" {
			msg += "\nResume: codex resume " + id
		}
		if session.Agent == "claude" {
			msg += "\nResume: claude --resume " + id
		}
	}
	if len(r.Sessions.Sessions) > 0 {
		msg += "\nRun resume commands from the receiving project."
	}
	return msg
}
func renderPreview(files []capture.PreviewFile, destination string) string {
	var out strings.Builder
	if destination != "" {
		fmt.Fprintln(&out, destination)
	}
	selected, excluded := 0, 0
	for _, file := range files {
		if file.Excluded {
			excluded++
		} else {
			selected++
		}
	}
	fmt.Fprintf(&out, "Would capture Git state and selected native sessions.\nSelected extra files: %d\n", selected)
	for _, file := range files {
		if file.Excluded {
			continue
		}
		label := file.Policy
		if file.Secret {
			label = "managed secret, encrypted"
		}
		fmt.Fprintf(&out, "  %s (%s)\n", terminalValue(file.RelativePath), terminalValue(label))
	}
	if excluded > 0 {
		fmt.Fprintf(&out, "Excluded secret candidates: %d\n", excluded)
		for _, file := range files {
			if file.Excluded {
				fmt.Fprintf(&out, "  %s (%s)\n", terminalValue(file.RelativePath), terminalValue(file.Policy))
			}
		}
		out.WriteString("Use --include-secret <path> to carry a permitted secret file encrypted.\n")
	}

	out.WriteString("Preview only; no files transferred or handoff baseline changed.")
	return out.String()
}

// Quote only untrusted values containing control/format characters. Layout newlines
// remain readable, and the structured JSON values retain their original bytes.
func terminalValue(value string) string { return otgerror.TerminalText(value) }

func terminalShellArgument(value string) string {
	if terminalValue(value) != value {
		// ANSI-C quoting is supported by bash and zsh. Unlike display quoting,
		// it keeps substitutions literal and preserves trailing newlines. Encode
		// non-ASCII bytes as fixed-width octal so terminal controls stay invisible
		// and invalid UTF-8 filenames still round-trip without locale conversion.
		var quoted strings.Builder
		quoted.WriteString("$'")
		for i := 0; i < len(value); i++ {
			b := value[i]
			switch {
			case b == '\'' || b == '\\':
				quoted.WriteByte('\\')
				quoted.WriteByte(b)
			case b >= 0x20 && b <= 0x7e:
				quoted.WriteByte(b)
			default:
				fmt.Fprintf(&quoted, "\\%03o", b)
			}
		}
		quoted.WriteByte('\'')
		return quoted.String()
	}
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}
func renderSessionPreview(sessions []agentsessions.Session) string {
	var out strings.Builder
	fmt.Fprintf(&out, "\nAgent sessions: %d", len(sessions))
	for _, s := range sessions {
		fmt.Fprintf(&out, "\n  %s %s", terminalValue(s.Agent), terminalValue(s.ID))
	}
	return out.String()
}

package cli

import (
	"fmt"
	"strings"

	"github.com/peerproblem/onthego/internal/capture"
	"github.com/peerproblem/onthego/internal/syncer"
)

func renderTransfer(action string, r syncer.Result) string {
	msg := action + " → " + r.Root
	if r.Backup != "" {
		msg += "\nPrevious workspace preserved in Git backup " + r.Backup + "; use onthego undo to restore it"
	}
	seenWarnings := map[string]bool{}
	for _, warning := range append(append([]string{}, r.Warnings...), r.Sessions.Warnings...) {
		if !seenWarnings[warning] {
			msg += "\nWarning: " + warning
			seenWarnings[warning] = true
		}
	}
	root := "'" + strings.ReplaceAll(r.Root, "'", "'\\''") + "'"
	msg += "\nContinue in the receiving workspace: cd " + root
	msg += fmt.Sprintf("\nAgent histories: %d imported, %d updated, %d unchanged.", r.Sessions.Imported, r.Sessions.Updated, r.Sessions.Skipped)
	for _, file := range r.Manifest.Files {
		if file.Secret {
			msg += "\nManaged secret file: " + file.RelativePath + " (included in subsequent handoffs)"
		}
	}
	seen := map[string]bool{}
	for _, session := range r.Sessions.Sessions {
		if session.ID == "" || seen[session.Agent+session.ID] {
			continue
		}
		seen[session.Agent+session.ID] = true
		id := "'" + strings.ReplaceAll(session.ID, "'", "'\\''") + "'"
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
		fmt.Fprintf(&out, "  %s (%s)\n", file.RelativePath, label)
	}
	if excluded > 0 {
		fmt.Fprintf(&out, "Excluded secret candidates: %d\n", excluded)
		for _, file := range files {
			if file.Excluded {
				fmt.Fprintf(&out, "  %s\n", file.RelativePath)
			}
		}
		out.WriteString("Use --include-secret <path> to carry a permitted secret file encrypted.\n")
	}

	out.WriteString("Preview only; no files transferred or handoff baseline changed.")
	return out.String()
}

// Command adapter exposes the production native-session adapter to the real-CLI
// compatibility harness. It handles only temporary, generated test sessions.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/peerproblem/onthego/internal/agentsessions"
)

func main() {
	if len(os.Args) != 6 {
		fmt.Fprintln(os.Stderr, "usage: adapter SOURCE_PROJECT TARGET_PROJECT SOURCE_HOME TARGET_HOME BUNDLE")
		os.Exit(2)
	}
	source := agentsessions.Roots{CodexHome: os.Args[3], ClaudeHome: os.Args[3]}
	target := agentsessions.Roots{CodexHome: os.Args[4], ClaudeHome: os.Args[4]}
	manifest, err := agentsessions.ExportWithOptions(os.Args[1], os.Args[5], source, agentsessions.Selection{All: true})
	if err == nil && len(manifest.Sessions) != 1 {
		err = fmt.Errorf("expected one generated session, got %d", len(manifest.Sessions))
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer os.RemoveAll(os.Args[5])
	result, err := agentsessions.Import(os.Args[5], os.Args[2], target)
	if err == nil && result.Imported != 1 {
		err = fmt.Errorf("expected one imported session: %+v", result)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if _, err := os.Stat(filepath.Join(os.Args[5], "manifest.json")); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if err := json.NewEncoder(os.Stdout).Encode(result); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

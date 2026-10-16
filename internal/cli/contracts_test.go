package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/peerproblem/onthego/internal/agentsessions"
	"github.com/peerproblem/onthego/internal/capture"
	"github.com/peerproblem/onthego/internal/otgerror"
	"github.com/peerproblem/onthego/internal/snapshot"
	"github.com/peerproblem/onthego/internal/syncer"
)

func TestPublicCodeExitContract(t *testing.T) {
	for _, tc := range []struct {
		code string
		exit int
	}{
		{"E_INTERNAL", 1}, {"E_INPUT", 2}, {"E_PRECONDITION", 3}, {"E_UNSUPPORTED_GIT", 3}, {"E_SECRET_POLICY", 3}, {"E_SESSION_FORMAT", 3},
		{"E_TARGET_AUTH", 4}, {"E_TARGET_UNAVAILABLE", 4}, {"E_PACKAGE_INVALID", 5}, {"E_RESTORE_FAILED", 5}, {"E_GENERATION_CONFLICT", 6}, {"E_SOURCE_CHANGED", 6}, {"E_CANCELED", 130},
	} {
		t.Run(tc.code, func(t *testing.T) {
			err := otgerror.New(tc.code, "example")
			if got := ExitCode(err); got != tc.exit {
				t.Fatalf("exit=%d, want %d", got, tc.exit)
			}
		})
	}
	for _, tc := range []struct {
		err  error
		code string
		exit int
	}{{errors.New("private detail"), "E_INTERNAL", 1}, {context.Canceled, "E_CANCELED", 130}, {fmt.Errorf("wrapped: %w", context.Canceled), "E_CANCELED", 130}, {context.DeadlineExceeded, "E_TARGET_UNAVAILABLE", 4}} {
		if got := otgerror.Public(tc.err); got.Code != tc.code || ExitCode(tc.err) != tc.exit {
			t.Fatalf("%v => %#v, exit=%d", tc.err, got, ExitCode(tc.err))
		}
	}
}
func TestTransferArgumentsValidateBeforeLocalState(t *testing.T) {
	for _, command := range [][]string{{"snapshot", "pack"}, {"snapshot", "pack", "--dry-run"}, {"push", "unconfigured"}, {"push", "unconfigured", "--dry-run"}, {"pull", "unconfigured"}, {"peer", "export"}} {
		for _, invalid := range [][]string{{"--session-days", "-1"}, {"--session", " "}, {"--session", ""}, {"--session", "one", "--all-sessions"}, {"--no-sessions", "--session", "one"}} {
			args := append(append([]string{}, command...), invalid...)
			t.Run(strings.Join(args, " "), func(t *testing.T) {
				c := (&app{}).rootCommand()
				c.SetArgs(args)
				c.SetOut(&bytes.Buffer{})
				c.SetErr(&bytes.Buffer{})
				err := c.Execute()
				if err == nil || otgerror.Public(err).Code != "E_INPUT" || ExitCode(err) != 2 {
					t.Fatalf("err=%v, public=%+v, exit=%d", err, otgerror.Public(err), ExitCode(err))
				}
			})
		}
	}
}
func TestRenderEscapesPathsAndSessionIDs(t *testing.T) {
	path := "notes\n\x1b[31m.txt"
	preview := renderPreview([]capture.PreviewFile{{RelativePath: path, Policy: "untracked"}}, "")
	result := renderTransfer("Received", syncer.Result{Root: "/safe/project", Manifest: snapshot.Manifest{Files: []snapshot.File{{RelativePath: path, Secret: true}}}, Sessions: agentsessions.ImportResult{Sessions: []agentsessions.Session{{Agent: "codex", ID: "id\n\x1b[31m"}}}})
	for _, got := range []string{preview, result} {
		if strings.Contains(got, "\x1b") || strings.Contains(got, "notes\n") || strings.Contains(got, "id\n") {
			t.Fatalf("unescaped control character: %q", got)
		}
		if !strings.Contains(got, `\n`) {
			t.Fatalf("missing visible escape: %q", got)
		}
	}
}

func TestPeerPackageCopyCancellationAndLimit(t *testing.T) {
	for _, partial := range []string{"", "partial ciphertext"} {
		t.Run(partial, func(t *testing.T) {
			input, writer := io.Pipe()
			defer input.Close()
			defer writer.Close()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- copyPeerPackage(ctx, io.Discard, input, 1024) }()
			if partial != "" {
				if _, err := writer.Write([]byte(partial)); err != nil {
					t.Fatal(err)
				}
			}
			cancel()
			select {
			case err := <-done:
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("copy=%v", err)
				}
			case <-time.After(time.Second):
				t.Fatal("copy did not return on cancellation")
			}
		})
	}
	for _, size := range []int{8, 9} {
		var output bytes.Buffer
		err := copyPeerPackage(context.Background(), &output, strings.NewReader(strings.Repeat("x", size)), 8)
		if size == 8 && err != nil {
			t.Fatal(err)
		}
		if size == 9 && (err == nil || otgerror.Public(err).Code != "E_PACKAGE_INVALID") {
			t.Fatalf("oversized copy=%v", err)
		}
	}
}
func TestNoSessionsFlagsAndConflictAlias(t *testing.T) {
	for _, args := range [][]string{{"push"}, {"pull"}, {"snapshot", "pack"}, {"peer", "export"}} {
		c, _, err := (&app{}).rootCommand().Find(args)
		if err != nil {
			t.Fatal(err)
		}
		if c.Flags().Lookup("no-sessions") == nil {
			t.Fatalf("%v missing no-sessions", args)
		}
	}
	for _, args := range [][]string{{"push"}, {"pull"}, {"snapshot", "restore"}, {"sessions", "recover"}} {
		c, _, err := (&app{}).rootCommand().Find(args)
		if err != nil {
			t.Fatal(err)
		}
		if c.Flags().Lookup("session-conflict") == nil {
			t.Fatalf("%v missing session-conflict", args)
		}
		alias := c.Flags().Lookup("sessions")
		if alias == nil || !alias.Hidden || alias.Deprecated == "" {
			t.Fatalf("%v alias=%#v", args, alias)
		}
	}
	if args := sessionArgs(agentsessions.Selection{None: true, RecentDays: 7}); strings.Join(args, " ") != "--no-sessions" {
		t.Fatalf("remote selection=%v", args)
	}
	for _, want := range []string{"snapshot-v3", "session-scope"} {
		found := false
		for _, capability := range protocolCapabilities {
			found = found || capability == want
		}
		if !found {
			t.Fatalf("missing capability %s", want)
		}
	}
}

func TestRecoveryNeededAppearsOnFirstLine(t *testing.T) {
	message := renderTransfer("Received", syncer.Result{Root: "/project", Sessions: agentsessions.ImportResult{Status: "recovery_needed"}})
	first, _, _ := strings.Cut(message, "\n")
	if !strings.Contains(first, "recovery needed") || !strings.Contains(message, "onthego sessions recover") {
		t.Fatalf("partial success is not clear: %q", message)
	}
}

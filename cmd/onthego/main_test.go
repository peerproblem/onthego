package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/peerproblem/onthego/internal/config"
	"github.com/peerproblem/onthego/internal/fsx"
	"github.com/peerproblem/onthego/internal/otgerror"
)

func TestStructuredErrorsPreserveContractWithoutPrivateCause(t *testing.T) {
	err := &otgerror.Error{
		Code: otgerror.CodeGenerationConflict, Message: "workspace changed", Stage: "apply",
		Preserved: "/private/recovery", Retryable: true, Cause: errors.New("token=DO_NOT_LEAK transcript text"),
	}
	var output bytes.Buffer
	writeError(&output, err, true)
	if strings.Contains(output.String(), "DO_NOT_LEAK") || strings.Contains(output.String(), "transcript") {
		t.Fatal("structured error leaked its private cause")
	}
	var got otgerror.Error
	if decodeErr := json.Unmarshal(output.Bytes(), &got); decodeErr != nil {
		t.Fatal(decodeErr)
	}
	if got.Code != err.Code || got.Message != err.Message || got.Stage != err.Stage || got.Preserved != err.Preserved || !got.Retryable {
		t.Fatalf("structured error lost contract fields: %#v", got)
	}
	output.Reset()
	writeError(&output, errors.New("token=DO_NOT_LEAK"), true)
	if strings.Contains(output.String(), "DO_NOT_LEAK") {
		t.Fatal("untyped error leaked private data")
	}
}

func TestMachineErrorSelection(t *testing.T) {
	for _, test := range []struct {
		args []string
		want bool
	}{
		{[]string{"push", "--json"}, true},
		{[]string{"--json=true", "push"}, true},
		{[]string{"push", "--json=1"}, true},
		{[]string{"--json", "--json=false", "push"}, false},
		{[]string{"peer", "apply"}, true},
		{[]string{"push"}, false},
		{[]string{"device", "add", "peer"}, false},
		{[]string{"push", "--", "--json"}, false},
	} {
		if got := machineErrors(test.args); got != test.want {
			t.Errorf("machineErrors(%q) = %v", test.args, got)
		}
	}
}

func TestSignalCancellationCleansPrivateTempAndAllowsRetry(t *testing.T) {
	for _, sig := range []os.Signal{os.Interrupt, syscall.SIGTERM} {
		t.Run(sig.String(), func(t *testing.T) {
			home := t.TempDir()
			cmd := exec.Command(os.Args[0], "-test.run=^TestMainSignalHelperProcess$")
			cmd.Env = append(os.Environ(), "ONTHEGO_SIGNAL_HELPER=1", "ONTHEGO_HOME="+home)
			stdout, err := cmd.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			var stderr bytes.Buffer
			cmd.Stderr = &stderr
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = cmd.Process.Kill() })
			ready := make(chan string, 1)
			go func() { line, _ := bufio.NewReader(stdout).ReadString('\n'); ready <- strings.TrimSpace(line) }()
			var dir string
			select {
			case dir = <-ready:
				if dir == "" {
					t.Fatal("helper never entered command")
				}
			case <-time.After(10 * time.Second):
				t.Fatal("helper did not become ready")
			}
			if err := cmd.Process.Signal(sig); err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() { done <- cmd.Wait() }()
			select {
			case err := <-done:
				var exit *exec.ExitError
				if !errors.As(err, &exit) || exit.ExitCode() != 130 {
					t.Fatalf("unexpected signal exit: %v (%s)", err, stderr.String())
				}
			case <-time.After(10 * time.Second):
				t.Fatal("signal failed to cancel command")
			}
			if _, err := os.Stat(dir); !os.IsNotExist(err) {
				t.Fatalf("plaintext temporary directory survived cancellation: %s", dir)
			}
			unlock, err := fsx.Lock(filepath.Join(home, "capture.lock"))
			if err != nil {
				t.Fatalf("retry failed after cancellation: %v", err)
			}
			unlock()
			var got otgerror.Error
			if err := json.Unmarshal(stderr.Bytes(), &got); err != nil || got.Message != "handoff canceled" {
				t.Fatalf("cancellation JSON malformed: %s (%v)", stderr.String(), err)
			}
		})
	}
}

func TestMainSignalHelperProcess(t *testing.T) {
	if os.Getenv("ONTHEGO_SIGNAL_HELPER") != "1" {
		return
	}
	home := os.Getenv("ONTHEGO_HOME")
	code := runCommand(func(ctx context.Context) error {
		unlock, err := fsx.Lock(filepath.Join(home, "capture.lock"))
		if err != nil {
			return err
		}
		defer unlock()
		dir, err := fsx.TempDir(home, "capture-")
		if err != nil {
			return err
		}
		defer fsx.ReleaseTemp(dir)
		if err := os.WriteFile(filepath.Join(dir, "private-session.jsonl"), []byte("private transcript"), 0o600); err != nil {
			return err
		}
		fmt.Println(dir)
		<-ctx.Done()
		return ctx.Err()
	}, []string{"--json"}, os.Stderr)
	os.Exit(code)
}

func TestSnapshotPackSignalCleansUpAndRetrySucceeds(t *testing.T) {
	git, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git is required for handoff lifecycle test")
	}
	base := t.TempDir()
	root, home, bin := filepath.Join(base, "project"), filepath.Join(base, "home"), filepath.Join(base, "bin")
	for _, dir := range []string{root, home, bin} {
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for _, args := range [][]string{{"init", "-b", "main"}, {"config", "user.name", "Lifecycle Test"}, {"config", "user.email", "test@example.invalid"}} {
		command := exec.Command(git, args...)
		command.Dir = root
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("git setup: %v: %s", err, output)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "source.txt"), []byte("initial\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := config.Save(root, config.Default()); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", "."}, {"-c", "core.hooksPath=/dev/null", "commit", "-m", "initial"}} {
		command := exec.Command(git, args...)
		command.Dir = root
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("git setup: %v: %s", err, output)
		}
	}
	// Stop a real snapshot pack after capture has acquired its locks and created
	// plaintext staging. exec keeps the stalled process owned by CommandContext.
	quotedGit := "'" + strings.ReplaceAll(git, "'", "'\\''") + "'"
	wrapper := "#!/bin/sh\nif [ \"$1\" = bundle ] && [ \"$ONTHEGO_TEST_BLOCK_BUNDLE\" = 1 ]; then\n  : > \"$ONTHEGO_TEST_BUNDLE_READY\"\n  exec /bin/sleep 60\nfi\nexec " + quotedGit + " \"$@\"\n"
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(wrapper), 0o700); err != nil {
		t.Fatal(err)
	}
	ready := filepath.Join(base, "bundle-ready")
	outputPath := filepath.Join(base, "snapshot.otg")
	arguments, _ := json.Marshal([]string{"snapshot", "pack", "--json", "--output", outputPath})
	env := append(os.Environ(), "ONTHEGO_CLI_HELPER=1", "ONTHEGO_TEST_CLI_ARGS="+string(arguments), "ONTHEGO_HOME="+home,
		"CODEX_HOME="+filepath.Join(base, "codex"), "CLAUDE_CONFIG_DIR="+filepath.Join(base, "claude"),
		"PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"), "ONTHEGO_TEST_BUNDLE_READY="+ready,
		"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null")
	cmd := exec.Command(os.Args[0], "-test.run=^TestCLIHelperProcess$")
	cmd.Dir, cmd.Env = root, append(env, "ONTHEGO_TEST_BLOCK_BUNDLE=1")
	var output bytes.Buffer
	cmd.Stdout, cmd.Stderr = &output, &output
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill() })
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	deadline := time.After(15 * time.Second)
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
waitForCapture:
	for {
		select {
		case <-tick.C:
			if _, err := os.Stat(ready); err == nil {
				break waitForCapture
			}
		case err := <-done:
			t.Fatalf("snapshot exited before capture was ready: %v: %s", err, output.String())
		case <-deadline:
			t.Fatal("snapshot capture never reached Git bundle")
		}
	}
	if err := cmd.Process.Signal(os.Interrupt); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.ExitCode() != 130 {
			t.Fatalf("unexpected snapshot cancellation: %v: %s", err, output.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("snapshot pack did not stop after SIGINT")
	}
	entries, err := os.ReadDir(filepath.Join(home, "tmp"))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.IsDir() && entry.Name() != ".leases" {
			t.Fatalf("plaintext staging survived canceled handoff: %s", entry.Name())
		}
	}
	retry := exec.Command(os.Args[0], "-test.run=^TestCLIHelperProcess$")
	retry.Dir, retry.Env = root, env
	if output, err := retry.CombinedOutput(); err != nil {
		t.Fatalf("snapshot pack retry failed after SIGINT: %v: %s", err, output)
	}
	if info, err := os.Stat(outputPath); err != nil || info.Size() == 0 {
		t.Fatalf("retry did not create encrypted package: %v", err)
	}
}

func TestCLIHelperProcess(t *testing.T) {
	if os.Getenv("ONTHEGO_CLI_HELPER") != "1" {
		return
	}
	var args []string
	if err := json.Unmarshal([]byte(os.Getenv("ONTHEGO_TEST_CLI_ARGS")), &args); err != nil {
		t.Fatal(err)
	}
	os.Args = append([]string{"onthego"}, args...)
	os.Exit(run())
}

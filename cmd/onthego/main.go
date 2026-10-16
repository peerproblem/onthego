package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/peerproblem/onthego/internal/cli"
	"github.com/peerproblem/onthego/internal/config"
	"github.com/peerproblem/onthego/internal/fsx"
	"github.com/peerproblem/onthego/internal/otgerror"
)

var (
	version = "dev"
	commit  = "none"
	builtAt = "unknown"
)

func main() {
	os.Exit(run())
}

func run() int {
	return runCommand(func(ctx context.Context) error {
		return cli.ExecuteContext(ctx, version, commit, builtAt)
	}, os.Args[1:], os.Stderr)
}

func runCommand(execute func(context.Context) error, args []string, stderr io.Writer) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		// The first signal requests cleanup; a second signal must retain the
		// default immediate-termination behavior if cleanup is stuck.
		stop()
	}()
	if home, err := config.DataRoot(); err == nil {
		if err := fsx.CleanupTemps(home, 24*time.Hour); err != nil {
			// Maintenance cannot make a healthy handoff fail. JSON and peer streams
			// remain machine-readable; private details are not included in warnings.
			if !machineErrors(args) {
				fmt.Fprintln(stderr, "Warning: could not clean expired private temporary directories.")
			}
		}
	}
	err := execute(ctx)
	if err == nil {
		return 0
	}
	if errors.Is(err, context.Canceled) || ctx.Err() != nil {
		writeError(stderr, otgerror.New(otgerror.CodeCanceled, "handoff canceled"), machineErrors(args))
		return 130
	}
	writeError(stderr, err, machineErrors(args))
	return cli.ExitCode(err)
}

func writeError(writer io.Writer, err error, machine bool) {
	if machine {
		_ = json.NewEncoder(writer).Encode(otgerror.Public(err))
		return
	}
	fmt.Fprintln(writer, otgerror.TerminalText(err.Error()))
}

func machineErrors(args []string) bool {
	jsonOutput := false
	commandSeen := false
	for _, arg := range args {
		if arg == "--" {
			break
		}
		if !commandSeen && !strings.HasPrefix(arg, "-") {
			commandSeen = true
			if arg == "peer" {
				return true
			}
		}
		if arg == "--json" {
			jsonOutput = true
		} else if strings.HasPrefix(arg, "--json=") {
			if value, err := strconv.ParseBool(strings.TrimPrefix(arg, "--json=")); err == nil {
				jsonOutput = value
			}
		}
	}
	return jsonOutput
}

package otgerror

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"testing"
)

func TestPublicFindsWrappedStructuredError(t *testing.T) {
	original := &Error{Code: CodeRestoreFailed, Message: "apply failed", Stage: "apply", Preserved: "/recovery", Cause: errors.New("private cause")}
	got := Public(fmt.Errorf("transport: %w", original))
	if got == original || got.Cause != nil || got.Code != original.Code || got.Stage != original.Stage || got.Preserved != original.Preserved {
		t.Fatalf("invalid public error: %#v", got)
	}
	if original.Cause == nil {
		t.Fatal("public conversion mutated the original error")
	}
}

func TestPublicCancellationAndTimeout(t *testing.T) {
	if got := Public(context.Canceled); got.Code != CodeCanceled || got.Message != "handoff canceled" {
		t.Fatalf("unexpected canceled error: %#v", got)
	}
	if got := Public(context.DeadlineExceeded); got.Code != CodeTargetUnavailable {
		t.Fatalf("unexpected timeout error: %#v", got)
	}
}

func TestTerminalTextQuotesOnlyUnsafeValues(t *testing.T) {
	for _, value := range []string{"plain/path", "한글 경로", "space and 'quote'"} {
		if got := TerminalText(value); got != value {
			t.Fatalf("safe value changed: %q => %q", value, got)
		}
	}
	for _, value := range []string{"line\nbreak", "ansi\x1b[31m", "tab\tvalue", "format\u202evalue", "invalid\x9bvalue"} {
		got := TerminalText(value)
		if got == value || got != strconv.Quote(value) {
			t.Fatalf("unsafe value not quoted: %q => %q", value, got)
		}
	}
	if got := Public(errors.New("private details")); got.Code != CodeInternal || got.Message != "unexpected error; rerun without --json for details" {
		t.Fatalf("untyped error=%#v", got)
	}
}

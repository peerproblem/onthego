package otgerror

import (
	"context"
	"errors"
	"fmt"
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
	if got := Public(context.Canceled); got.Code != CodePrecondition || got.Message != "handoff canceled" {
		t.Fatalf("unexpected canceled error: %#v", got)
	}
	if got := Public(context.DeadlineExceeded); got.Code != CodeTargetUnavailable {
		t.Fatalf("unexpected timeout error: %#v", got)
	}
}

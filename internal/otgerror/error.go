package otgerror

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"unicode"
	"unicode/utf8"
)

const (
	CodeSourceChanged      = "E_SOURCE_CHANGED"
	CodeUnsupportedGit     = "E_UNSUPPORTED_GIT"
	CodeSecretPolicy       = "E_SECRET_POLICY"
	CodePackageInvalid     = "E_PACKAGE_INVALID"
	CodeTargetAuth         = "E_TARGET_AUTH"
	CodeTargetUnavailable  = "E_TARGET_UNAVAILABLE"
	CodeGenerationConflict = "E_GENERATION_CONFLICT"
	CodeSessionFormat      = "E_SESSION_FORMAT"
	CodeRestoreFailed      = "E_RESTORE_FAILED"
	CodeInput              = "E_INPUT"
	CodeInternal           = "E_INTERNAL"
	CodeCanceled           = "E_CANCELED"
	CodePrecondition       = "E_PRECONDITION"
)

type Error struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	Retryable bool   `json:"retryable"`
	Stage     string `json:"stage,omitempty"`
	Preserved string `json:"preserved_path,omitempty"`
	Cause     error  `json:"-"`
}

func (e *Error) Error() string {
	message := e.Message
	if e.Cause != nil {
		message += ": " + e.Cause.Error()
	}
	if e.Stage != "" {
		return fmt.Sprintf("%s at %s: %s", e.Code, e.Stage, message)
	}
	return fmt.Sprintf("%s: %s", e.Code, message)
}

func (e *Error) Unwrap() error { return e.Cause }

func New(code, message string) *Error {
	return &Error{Code: code, Message: message}
}

func Wrap(code, message string, cause error) *Error {
	return &Error{Code: code, Message: message, Cause: cause}
}

// Public returns the structured error contract without exposing an underlying
// subprocess error, credential-bearing URL, or native session contents.
func Public(err error) *Error {
	if errors.Is(err, context.Canceled) {
		return New(CodeCanceled, "handoff canceled")
	}
	var typed *Error
	if errors.As(err, &typed) {
		return &Error{Code: typed.Code, Message: typed.Message, Retryable: typed.Retryable, Stage: typed.Stage, Preserved: typed.Preserved}
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return New(CodeTargetUnavailable, "handoff timed out")
	}
	return New(CodeInternal, "unexpected error; rerun without --json for details")
}

// TerminalText escapes untrusted control characters without changing JSON data.
func TerminalText(value string) string {
	if !utf8.ValidString(value) {
		return strconv.Quote(value)
	}
	for _, r := range value {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || r == '\u2028' || r == '\u2029' {
			return strconv.Quote(value)
		}
	}
	return value
}

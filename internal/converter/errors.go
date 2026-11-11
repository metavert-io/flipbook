package converter

import (
	"errors"
	"fmt"
	"strings"
)

var (
	ErrUnsupportedFormat  = errors.New("unsupported document format")
	ErrEncryptedPDF       = errors.New("encrypted PDF requires a password")
	ErrCorruptPDF         = errors.New("corrupt or invalid PDF")
	ErrQuotaExceeded      = errors.New("conversion quota exceeded")
	ErrArtifactValidation = errors.New("conversion artifact validation failed")
	ErrInvalidOptions     = errors.New("invalid converter options")
	ErrInvalidStaging     = errors.New("invalid staging directory")
	ErrTimeout            = errors.New("time limit exceeded")
)

// ConversionError adds a stable category and optional page number to an error.
type ConversionError struct {
	Kind  error
	Op    string
	Page  int
	Cause error
}

func (e *ConversionError) Error() string {
	parts := []string{"conversion"}
	if e.Op != "" {
		parts = append(parts, e.Op)
	}
	if e.Page > 0 {
		parts = append(parts, fmt.Sprintf("page %d", e.Page))
	}
	message := strings.Join(parts, " ")
	if e.Kind != nil {
		message += ": " + e.Kind.Error()
	}
	if e.Cause != nil {
		message += ": " + e.Cause.Error()
	}
	return message
}

func (e *ConversionError) Unwrap() []error {
	var errs []error
	if e.Kind != nil {
		errs = append(errs, e.Kind)
	}
	if e.Cause != nil {
		errs = append(errs, e.Cause)
	}
	return errs
}

func newConversionError(kind error, op string, cause error) error {
	return &ConversionError{Kind: kind, Op: op, Cause: cause}
}

func pageConversionError(kind error, op string, page int, cause error) error {
	return &ConversionError{Kind: kind, Op: op, Page: page, Cause: cause}
}

func classifyPDFToolError(op string, cause error, output []byte) error {
	text := strings.ToLower(string(output))
	kind := ErrCorruptPDF
	switch {
	case strings.Contains(text, "incorrect password"),
		strings.Contains(text, "password required"),
		strings.Contains(text, "encrypted") && strings.Contains(text, "error"):
		kind = ErrEncryptedPDF
	}
	message := strings.TrimSpace(string(output))
	if len(message) > 4096 {
		message = message[:4096]
	}
	if message == "" {
		return newConversionError(kind, op, cause)
	}
	return newConversionError(kind, op, fmt.Errorf("%w: %s", cause, message))
}

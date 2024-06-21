package logging

import "log/slog"

// Error creates a new slog.Attr that represents an error.
func Error(err error) slog.Attr {
	if err == nil {
		return slog.Attr{}
	}

	// TODO: Extract special attrs here.

	// Datadog expects errors to be in the error.message field to be
	// eligible for FTS indexing, so if we put it in a field called
	// "message" in a group called "error" it will end up in the field
	// called error.message in the final JSON output.
	attrs := []any{slog.String("message", err.Error())}
	return slog.Group("error", attrs...)
}

// Package lint defines the diagnostic types shared between the typescript-go
// fork's wrapper layer and any linter built on top of it. The package
// deliberately depends only on the standard library so consumers can take a
// dependency on it without pulling in the rest of the compiler.
package lint

// Severity is the level at which a diagnostic is reported. The set is
// intentionally small; richer per-rule configuration belongs in the linter,
// not here.
type Severity string

const (
	// SeverityError indicates a finding that should fail a build or a CI gate.
	SeverityError Severity = "error"
	// SeverityWarning indicates a finding that should not fail a build.
	SeverityWarning Severity = "warning"
)

// SourceRange identifies a contiguous span within a source file using
// one-indexed line and one-indexed UTF-16 column positions, matching the
// Language Server Protocol convention so diagnostics interoperate with
// editor tooling without translation.
type SourceRange struct {
	File        string
	StartLine   int
	StartColumn int
	EndLine     int
	EndColumn   int
}

// Diagnostic is a single finding reported by a rule. It carries the location,
// the rule that produced it, the severity at which it was emitted, and a
// human-readable message. Formatters render this; rules produce it.
type Diagnostic struct {
	Range    SourceRange
	RuleID   string
	Severity Severity
	Message  string
}

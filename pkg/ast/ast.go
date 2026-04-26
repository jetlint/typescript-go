// Package ast is the wrapper-layer entry point for AST node access from
// outside the typescript-go module. Consumers must use the types and
// functions exported here rather than importing internal/ast directly.
//
// This package is intentionally minimal in this revision. Concrete node
// kinds and visitor helpers will be added as rule implementations require
// them. The discipline of "expose only what a rule actually needs" keeps
// the wrapper surface small and predictable across upstream churn.
package ast

// Kind identifies the syntactic category of an AST node. The numeric values
// are not stable across upstream typescript-go revisions; consumers must
// compare against the Kind* constants defined here, never against integer
// literals.
type Kind int

// Node is the wrapper-layer view of an AST node. Concrete accessors are
// added on demand as rules require them.
type Node interface {
	Kind() Kind
}

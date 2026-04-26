// Package checker is the wrapper-layer entry point for type-aware queries
// against a typescript-go program. Consumers must use the types and methods
// exported here rather than importing internal/checker directly.
//
// This package is intentionally minimal in this revision. Concrete query
// methods (TypeOf, SymbolOf, ContextualType, SignatureOf, and the helper
// predicates listed in the design) will be added as rule implementations
// require them. The discipline of "expose only what a rule actually needs"
// keeps the wrapper surface small and predictable across upstream churn.
package checker

import "github.com/microsoft/typescript-go/pkg/ast"

// Type is the wrapper-layer view of a checker type. Helpers are added on
// demand as rules require them.
type Type interface {
	String() string
}

// Symbol is the wrapper-layer view of a checker symbol.
type Symbol interface {
	Name() string
}

// Signature is the wrapper-layer view of a callable signature.
type Signature interface {
	ReturnType() Type
}

// Checker performs type-aware queries against a Program. Concrete methods
// are added on demand.
type Checker interface {
	TypeOf(node ast.Node) Type
}

// Program is a loaded TypeScript program. Concrete methods are added on
// demand.
type Program interface {
	Checker() Checker
}

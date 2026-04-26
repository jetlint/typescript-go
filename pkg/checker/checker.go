// Package checker is the wrapper-layer entry point for type-aware queries
// against a typescript-go program. Consumers must use the types and
// methods exported here rather than importing internal/checker directly.
//
// The surface grows as rule implementations require new primitives.
// Each addition documents the upstream symbol it delegates to so future
// renames can be located by grep.
package checker

import (
	"context"
	"fmt"
	"os"

	"github.com/microsoft/typescript-go/internal/ast"
	"github.com/microsoft/typescript-go/internal/bundled"
	"github.com/microsoft/typescript-go/internal/checker"
	"github.com/microsoft/typescript-go/internal/compiler"
	"github.com/microsoft/typescript-go/internal/scanner"
	"github.com/microsoft/typescript-go/internal/tsoptions"
	"github.com/microsoft/typescript-go/internal/tspath"
	"github.com/microsoft/typescript-go/internal/vfs"
	"github.com/microsoft/typescript-go/internal/vfs/osvfs"
)

// Program is a loaded TypeScript program. It owns the parsed source
// files and the type checker bound to those files.
type Program struct {
	inner   *compiler.Program
	checker *checker.Checker
	cleanup func()
}

// LoadProgram constructs a Program from a tsconfig.json on disk. Source
// files are parsed eagerly; the type checker is constructed eagerly so
// the first lint pass does not pay the construction cost.
func LoadProgram(tsconfigPath string) (*Program, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return nil, fmt.Errorf("getcwd: %w", err)
	}
	cwd = tspath.NormalizePath(cwd)

	fs := bundled.WrapFS(osvfs.FS())
	libPath := bundled.LibPath()

	host := compiler.NewCachedFSCompilerHost(cwd, fs, libPath, nil, nil)

	parseHost := &parseConfigHost{fs: fs, cwd: cwd}
	configFileText, ok := fs.ReadFile(tsconfigPath)
	if !ok {
		return nil, fmt.Errorf("read %s: file not readable", tsconfigPath)
	}
	configFilePath := tspath.ToPath(tsconfigPath, cwd, fs.UseCaseSensitiveFileNames())
	jsonValue, diags := tsoptions.ParseConfigFileTextToJson(tsconfigPath, configFilePath, configFileText)
	if len(diags) > 0 {
		return nil, fmt.Errorf("parse %s: %d tsconfig diagnostic(s)", tsconfigPath, len(diags))
	}
	parsed := tsoptions.ParseJsonConfigFileContent(jsonValue, parseHost,
		tspath.GetDirectoryPath(tsconfigPath), nil, tsconfigPath, nil, nil, nil)
	if parsed == nil {
		return nil, fmt.Errorf("could not parse tsconfig content at %s", tsconfigPath)
	}

	prog := compiler.NewProgram(compiler.ProgramOptions{
		Config: parsed,
		Host:   host,
	})
	if prog == nil {
		return nil, fmt.Errorf("compiler.NewProgram returned nil for %s", tsconfigPath)
	}

	chk, cleanup := prog.GetTypeChecker(context.Background())
	return &Program{inner: prog, checker: chk, cleanup: cleanup}, nil
}

// Close releases checker resources. Safe to call multiple times.
func (p *Program) Close() {
	if p.cleanup != nil {
		p.cleanup()
		p.cleanup = nil
	}
}

// SourceFiles returns the program's user source files (declaration files
// and the bundled lib are excluded so rule walks visit only the code
// the user actually wrote).
func (p *Program) SourceFiles() []*SourceFile {
	files := p.inner.SourceFiles()
	out := make([]*SourceFile, 0, len(files))
	for _, f := range files {
		if f.IsDeclarationFile {
			continue
		}
		out = append(out, &SourceFile{inner: f})
	}
	return out
}

// SourceFileByPath returns the source file at the given absolute path,
// or nil when the file is not part of the program.
func (p *Program) SourceFileByPath(path string) *SourceFile {
	abs := path
	for _, f := range p.inner.SourceFiles() {
		if f.FileName() == abs {
			return &SourceFile{inner: f}
		}
	}
	return nil
}

// Checker returns the type checker bound to this program.
func (p *Program) Checker() *Checker { return &Checker{inner: p.checker} }

// HasTypeErrors reports whether the program contains any syntactic or
// semantic diagnostics across its user source files. Used by the
// linter to surface a degraded-mode signal: if the type graph is
// unsound, lint diagnostics built on it may be wrong, and the AI
// agent or human consumer needs to know.
func (p *Program) HasTypeErrors() bool {
	for _, f := range p.inner.SourceFiles() {
		if f.IsDeclarationFile {
			continue
		}
		if len(p.inner.GetSyntacticDiagnostics(context.Background(), f)) > 0 {
			return true
		}
		if len(p.inner.GetSemanticDiagnostics(context.Background(), f)) > 0 {
			return true
		}
	}
	return false
}

// SourceFile is the wrapper view of a parsed source file.
type SourceFile struct{ inner *ast.SourceFile }

// Path returns the absolute path of the source file.
func (s *SourceFile) Path() string { return s.inner.FileName() }

// Walk invokes visit for every node in the file in depth-first order.
// Returning false from visit skips the current node's children.
func (s *SourceFile) Walk(visit func(*Node) bool) {
	walk(s.inner.AsNode(), visit)
}

func walk(n *ast.Node, visit func(*Node) bool) {
	if n == nil {
		return
	}
	if !visit(&Node{inner: n}) {
		return
	}
	n.ForEachChild(func(child *ast.Node) bool {
		walk(child, visit)
		return false
	})
}

// Node is the wrapper view of an AST node.
type Node struct{ inner *ast.Node }

// Kind returns the syntactic kind of the node.
func (n *Node) Kind() Kind { return Kind(n.inner.Kind) }

// Pos returns the start offset of the node within its source file.
func (n *Node) Pos() int { return n.inner.Pos() }

// End returns the end offset of the node within its source file.
func (n *Node) End() int { return n.inner.End() }

// Parent returns the parent node, or nil for the source file root.
func (n *Node) Parent() *Node {
	if n == nil || n.inner == nil || n.inner.Parent == nil {
		return nil
	}
	return &Node{inner: n.inner.Parent}
}

// Inner returns the underlying *ast.Node. Reserved for the wrapper
// itself; rule packages must not reach into the result.
func (n *Node) Inner() *ast.Node { return n.inner }

// ForEachChild invokes visit on every direct child of n. Stops early
// when visit returns true, mirroring the standard tsgo visitor
// convention.
func (n *Node) ForEachChild(visit func(*Node) bool) bool {
	if n == nil || n.inner == nil {
		return false
	}
	return n.inner.ForEachChild(func(child *ast.Node) bool {
		return visit(&Node{inner: child})
	})
}

// FirstChild returns the first direct child of n, or nil when n has no
// children.
func (n *Node) FirstChild() *Node {
	var first *Node
	n.ForEachChild(func(c *Node) bool {
		first = c
		return true
	})
	return first
}

// SourceRange returns the (file, startLine, startCol, endLine, endCol)
// coordinates of the node, with line and column numbers 1-indexed and
// columns expressed as UTF-16 code units (matching the LSP convention).
//
// The start position is the position of the node's first significant
// token (skipping leading whitespace and comments), which is what
// editors and humans expect when shown a diagnostic location. The end
// position is the node's `End()`.
func (n *Node) SourceRange() (file string, startLine, startCol, endLine, endCol int) {
	sf := ast.GetSourceFileOfNode(n.inner)
	if sf == nil {
		return "", 0, 0, 0, 0
	}
	startPos := scanner.GetTokenPosOfNode(n.inner, sf, false)
	startLine0, startCol0 := scanner.GetECMALineAndUTF16CharacterOfPosition(sf, startPos)
	endLine0, endCol0 := scanner.GetECMALineAndUTF16CharacterOfPosition(sf, n.inner.End())
	return sf.FileName(), startLine0 + 1, int(startCol0) + 1, endLine0 + 1, int(endCol0) + 1
}

// Kind identifies the syntactic category of an AST node. Numeric values
// are not stable across tsgo upstream revisions; consumers must compare
// against the Kind* constants exposed here, never against integer
// literals.
type Kind int

// Selected Kind constants exposed for rule authors. The set grows on
// demand as rules require additional kinds. The values delegate to the
// internal/ast package; code in this file is the only place that
// references those internal constants.
const (
	KindCallExpression           = Kind(ast.KindCallExpression)
	KindExpressionStatement      = Kind(ast.KindExpressionStatement)
	KindBinaryExpression         = Kind(ast.KindBinaryExpression)
	KindIdentifier               = Kind(ast.KindIdentifier)
	KindPropertyAccessExpression = Kind(ast.KindPropertyAccessExpression)
	KindTemplateExpression       = Kind(ast.KindTemplateExpression)
	KindTemplateSpan             = Kind(ast.KindTemplateSpan)
	KindIfStatement              = Kind(ast.KindIfStatement)
	KindConditionalExpression    = Kind(ast.KindConditionalExpression)
	KindWhileStatement           = Kind(ast.KindWhileStatement)
	KindForStatement             = Kind(ast.KindForStatement)
	KindAwaitExpression          = Kind(ast.KindAwaitExpression)
	KindReturnStatement          = Kind(ast.KindReturnStatement)
	KindArrowFunction            = Kind(ast.KindArrowFunction)
	KindFunctionDeclaration      = Kind(ast.KindFunctionDeclaration)
	KindFunctionExpression       = Kind(ast.KindFunctionExpression)
	KindMethodDeclaration        = Kind(ast.KindMethodDeclaration)
	KindVariableDeclaration      = Kind(ast.KindVariableDeclaration)
	KindStringLiteral            = Kind(ast.KindStringLiteral)
	KindParenthesizedExpression  = Kind(ast.KindParenthesizedExpression)
	KindVoidExpression           = Kind(ast.KindVoidExpression)
)

// Checker performs type-aware queries against a Program.
type Checker struct{ inner *checker.Checker }

// TypeOf returns the type of the given node.
func (c *Checker) TypeOf(n *Node) *Type {
	if n == nil || n.inner == nil {
		return nil
	}
	t := c.inner.GetTypeAtLocation(n.inner)
	if t == nil {
		return nil
	}
	return &Type{inner: t, checker: c.inner}
}

// ContextualTypeForArgument returns the parameter type a call
// expression's argument at argIndex is contextually expected to
// satisfy. Useful for rules that check argument shape against the
// callee's signature without needing to resolve the signature manually.
func (c *Checker) ContextualTypeForArgument(call *Node, argIndex int) *Type {
	if call == nil || call.inner == nil {
		return nil
	}
	t := c.inner.GetContextualTypeForArgumentAtIndex(call.inner, argIndex)
	if t == nil {
		return nil
	}
	return &Type{inner: t, checker: c.inner}
}

// IsAsyncFunction reports whether the node is a function-like AST node
// declared with the `async` modifier.
func IsAsyncFunction(n *Node) bool {
	if n == nil || n.inner == nil {
		return false
	}
	return ast.IsAsyncFunction(n.inner)
}

// CallArguments returns the argument expressions of a CallExpression,
// in source order. Returns nil for non-call nodes.
func (n *Node) CallArguments() []*Node {
	if n == nil || n.inner == nil {
		return nil
	}
	if !ast.IsCallExpression(n.inner) {
		return nil
	}
	args := n.inner.Arguments()
	out := make([]*Node, 0, len(args))
	for _, a := range args {
		out = append(out, &Node{inner: a})
	}
	return out
}

// SymbolOf returns the symbol the node refers to, or nil.
func (c *Checker) SymbolOf(n *Node) *Symbol {
	if n == nil || n.inner == nil {
		return nil
	}
	s := c.inner.GetSymbolAtLocation(n.inner)
	if s == nil {
		return nil
	}
	return &Symbol{inner: s, checker: c.inner}
}

// Type is the wrapper view of a checker type. Helpers are added on
// demand as rules require them.
type Type struct {
	inner   *checker.Type
	checker *checker.Checker
}

// String returns a human-readable rendering of the type.
func (t *Type) String() string {
	if t == nil || t.inner == nil {
		return ""
	}
	return t.checker.TypeToString(t.inner)
}

// IsAny reports whether the type is `any`.
func (t *Type) IsAny() bool {
	if t == nil || t.inner == nil {
		return false
	}
	return t.inner.Flags()&checker.TypeFlagsAny != 0
}

// IsUnknown reports whether the type is `unknown`.
func (t *Type) IsUnknown() bool {
	if t == nil || t.inner == nil {
		return false
	}
	return t.inner.Flags()&checker.TypeFlagsUnknown != 0
}

// IsBooleanLike reports whether the type is boolean, true, false, or a
// union over boolean literals.
func (t *Type) IsBooleanLike() bool {
	if t == nil || t.inner == nil {
		return false
	}
	return t.inner.Flags()&checker.TypeFlagsBooleanLike != 0
}

// IsStringLike reports whether the type is a string-shaped type.
func (t *Type) IsStringLike() bool {
	if t == nil || t.inner == nil {
		return false
	}
	return t.inner.Flags()&checker.TypeFlagsStringLike != 0
}

// IsNullOrUndefined reports whether the type contains null or undefined.
func (t *Type) IsNullOrUndefined() bool {
	if t == nil || t.inner == nil {
		return false
	}
	return t.inner.Flags()&checker.TypeFlagsNullable != 0
}

// IsVoid reports whether the type is exactly the `void` type.
func (t *Type) IsVoid() bool {
	if t == nil || t.inner == nil {
		return false
	}
	return t.inner.Flags()&checker.TypeFlagsVoid != 0
}

// IsVoidLike reports whether the type is `void`, `undefined`, or a
// union of those (the "callback returning void" position in
// type-aware-rule shorthand).
func (t *Type) IsVoidLike() bool {
	if t == nil || t.inner == nil {
		return false
	}
	return t.inner.Flags()&checker.TypeFlagsVoidLike != 0
}

// CallSignatures returns the call signatures of the type. A function
// type yields its single signature; an overloaded function yields all
// of them; a non-callable yields an empty slice.
func (t *Type) CallSignatures() []*Signature {
	if t == nil || t.inner == nil {
		return nil
	}
	sigs := t.checker.GetCallSignatures(t.inner)
	out := make([]*Signature, 0, len(sigs))
	for _, s := range sigs {
		out = append(out, &Signature{inner: s, checker: t.checker})
	}
	return out
}

// IsUnion reports whether the type is a union.
func (t *Type) IsUnion() bool {
	if t == nil || t.inner == nil {
		return false
	}
	return t.inner.Flags()&checker.TypeFlagsUnion != 0
}

// UnionMembers returns the constituent types of a union, or a single-
// element slice for non-union types.
func (t *Type) UnionMembers() []*Type {
	if t == nil || t.inner == nil {
		return nil
	}
	if t.inner.Flags()&checker.TypeFlagsUnion == 0 {
		return []*Type{t}
	}
	parts := t.inner.Types()
	out := make([]*Type, 0, len(parts))
	for _, p := range parts {
		out = append(out, &Type{inner: p, checker: t.checker})
	}
	return out
}

// IsThenable reports whether the type has a callable `then` property —
// the same definition the TypeScript checker uses to decide whether a
// value is awaitable. Promises, generic Promise<T>, and any user-defined
// thenable all satisfy this predicate.
func (t *Type) IsThenable() bool {
	if t == nil || t.inner == nil {
		return false
	}
	// Primitives are never thenable.
	if t.inner.Flags()&checker.TypeFlagsPrimitive != 0 {
		return false
	}
	props := t.checker.GetApparentProperties(t.inner)
	for _, p := range props {
		if p.Name != "then" {
			continue
		}
		propType := t.checker.GetTypeOfSymbol(p)
		if propType == nil {
			continue
		}
		if len(t.checker.GetCallSignatures(propType)) > 0 {
			return true
		}
	}
	return false
}

// ArrayElementType returns the element type of an array-like type, or
// nil when the type is not array-like. Used by rules that need to
// reason about the contents of arrays (e.g., the toString of `T[]`
// invokes `T.toString` on each element).
func (t *Type) ArrayElementType() *Type {
	if t == nil || t.inner == nil {
		return nil
	}
	elem := t.checker.GetElementTypeOfArrayType(t.inner)
	if elem == nil {
		return nil
	}
	return &Type{inner: elem, checker: t.checker}
}

// HasOwnToString reports whether the type declares its own toString
// method (i.e., one that is not the default Object.prototype.toString).
// Used by the no-base-to-string rule to distinguish meaningful string
// conversion from "[object Object]" garbage.
func (t *Type) HasOwnToString() bool {
	if t == nil || t.inner == nil {
		return false
	}
	// Primitives have meaningful string conversion.
	if t.inner.Flags()&checker.TypeFlagsPrimitive != 0 {
		return true
	}
	props := t.checker.GetApparentProperties(t.inner)
	for _, p := range props {
		if p.Name != "toString" {
			continue
		}
		// A symbol whose declarations all live in the default lib (the
		// platform's Object.prototype.toString) is the unhelpful one. Any
		// other declaration site means the user (or a library) supplied
		// their own toString.
		for _, decl := range p.Declarations {
			sf := ast.GetSourceFileOfNode(decl)
			if sf == nil {
				continue
			}
			if !sf.IsDeclarationFile {
				return true
			}
			// Heuristic: declarations in user code (non-declaration files)
			// always count; declarations in lib.*.d.ts do not. This is
			// imperfect for user-supplied .d.ts files that augment Object,
			// but matches the typescript-eslint rule's behavior.
			name := sf.FileName()
			if !isLikelyDefaultLib(name) {
				return true
			}
		}
		return false
	}
	return false
}

func isLikelyDefaultLib(path string) bool {
	// The bundled TypeScript libs all have filenames matching lib.*.d.ts.
	// User .d.ts files typically don't.
	const prefix = "lib."
	const suffix = ".d.ts"
	base := path
	if i := lastIndex(base, '/'); i >= 0 {
		base = base[i+1:]
	}
	return len(base) > len(prefix)+len(suffix) &&
		base[:len(prefix)] == prefix &&
		base[len(base)-len(suffix):] == suffix
}

func lastIndex(s string, c byte) int {
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] == c {
			return i
		}
	}
	return -1
}

// Inner returns the underlying *checker.Type. Reserved for the wrapper
// itself; rules must not reach into the result.
func (t *Type) Inner() *checker.Type { return t.inner }

// Signature is the wrapper view of a callable signature.
type Signature struct {
	inner   *checker.Signature
	checker *checker.Checker
}

// ReturnType returns the signature's return type.
func (s *Signature) ReturnType() *Type {
	if s == nil || s.inner == nil {
		return nil
	}
	t := s.checker.GetReturnTypeOfSignature(s.inner)
	if t == nil {
		return nil
	}
	return &Type{inner: t, checker: s.checker}
}

// Symbol is the wrapper view of a checker symbol.
type Symbol struct {
	inner   *ast.Symbol
	checker *checker.Checker
}

// Name returns the symbol's declared name.
func (s *Symbol) Name() string {
	if s == nil || s.inner == nil {
		return ""
	}
	return s.inner.Name
}

// --- internal helpers ---

type parseConfigHost struct {
	fs  vfs.FS
	cwd string
}

func (h *parseConfigHost) FS() vfs.FS                  { return h.fs }
func (h *parseConfigHost) GetCurrentDirectory() string { return h.cwd }

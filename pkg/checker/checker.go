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
	"github.com/microsoft/typescript-go/internal/core"
	"github.com/microsoft/typescript-go/internal/parser"
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
//
// Panics from the underlying tsgo compiler (currently common for some
// edge-case tsconfig shapes — project references being the headline
// example) are recovered into a structured error so the caller can
// surface a clean diagnostic instead of a stack trace.
func LoadProgram(tsconfigPath string) (prog *Program, err error) {
	defer func() {
		if r := recover(); r != nil {
			prog = nil
			err = fmt.Errorf("typescript-go panicked while loading %s: %v (this is usually an unsupported tsconfig shape such as project references; consider pointing at a child tsconfig.app.json instead of a project-references root)", tsconfigPath, r)
		}
	}()
	return loadProgramImpl(tsconfigPath)
}

func loadProgramImpl(tsconfigPath string) (*Program, error) {
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
	KindObjectLiteralExpression  = Kind(ast.KindObjectLiteralExpression)
	KindArrayLiteralExpression   = Kind(ast.KindArrayLiteralExpression)
	KindPropertyAssignment       = Kind(ast.KindPropertyAssignment)
	KindNoSubstitutionTemplateLiteral = Kind(ast.KindNoSubstitutionTemplateLiteral)
	KindNumericLiteral           = Kind(ast.KindNumericLiteral)
	KindEqualsToken              = Kind(ast.KindEqualsToken)
	KindCommaToken               = Kind(ast.KindCommaToken)
	KindBarBarToken              = Kind(ast.KindBarBarToken)
	KindAmpersandAmpersandToken  = Kind(ast.KindAmpersandAmpersandToken)
	KindQuestionQuestionToken    = Kind(ast.KindQuestionQuestionToken)
	KindSpreadElement            = Kind(ast.KindSpreadElement)
	KindPlusToken                = Kind(ast.KindPlusToken)
	KindPlusEqualsToken          = Kind(ast.KindPlusEqualsToken)
	KindTaggedTemplateExpression = Kind(ast.KindTaggedTemplateExpression)
	KindNewExpression            = Kind(ast.KindNewExpression)
	KindPrefixUnaryExpression    = Kind(ast.KindPrefixUnaryExpression)
	KindDoStatement              = Kind(ast.KindDoStatement)
	KindExclamationToken         = Kind(ast.KindExclamationToken)
	KindSpreadAssignment         = Kind(ast.KindSpreadAssignment)
	KindThrowStatement           = Kind(ast.KindThrowStatement)
	KindForOfStatement           = Kind(ast.KindForOfStatement)
	KindForInStatement           = Kind(ast.KindForInStatement)
	KindSwitchStatement          = Kind(ast.KindSwitchStatement)
	KindCaseClause               = Kind(ast.KindCaseClause)
	KindElementAccessExpression  = Kind(ast.KindElementAccessExpression)
	KindAsExpression             = Kind(ast.KindAsExpression)
	KindTypeAssertionExpression  = Kind(ast.KindTypeAssertionExpression)
	KindNonNullExpression        = Kind(ast.KindNonNullExpression)
	KindDeleteExpression         = Kind(ast.KindDeleteExpression)
	KindMinusToken                       = Kind(ast.KindMinusToken)
	KindEqualsEqualsToken                = Kind(ast.KindEqualsEqualsToken)
	KindEqualsEqualsEqualsToken          = Kind(ast.KindEqualsEqualsEqualsToken)
	KindExclamationEqualsToken           = Kind(ast.KindExclamationEqualsToken)
	KindExclamationEqualsEqualsToken     = Kind(ast.KindExclamationEqualsEqualsToken)
	KindLessThanToken                    = Kind(ast.KindLessThanToken)
	KindLessThanEqualsToken              = Kind(ast.KindLessThanEqualsToken)
	KindGreaterThanToken                 = Kind(ast.KindGreaterThanToken)
	KindGreaterThanEqualsToken           = Kind(ast.KindGreaterThanEqualsToken)
	KindTrueKeyword              = Kind(ast.KindTrueKeyword)
	KindFalseKeyword             = Kind(ast.KindFalseKeyword)
	KindNullKeyword              = Kind(ast.KindNullKeyword)
)

// ParseFile parses a single source file from text without loading a full
// program. Useful for tooling that needs the AST but no type information,
// e.g. extracting test fixtures from a JS/TS source file. The returned
// SourceFile is detached from any program; type queries against it are
// not available.
func ParseFile(filePath, sourceText string) *SourceFile {
	opts := ast.SourceFileParseOptions{
		FileName: filePath,
		Path:     tspath.Path(filePath),
	}
	kind := core.GetScriptKindFromFileName(filePath)
	if kind == core.ScriptKindUnknown {
		kind = core.ScriptKindTS
	}
	inner := parser.ParseSourceFile(opts, sourceText, kind)
	return &SourceFile{inner: inner}
}

// LiteralText returns the parsed text value of a literal-bearing node
// (StringLiteral, NoSubstitutionTemplateLiteral, NumericLiteral,
// Identifier, etc.). Empty string for non-literal nodes. Used by
// tooling that needs to extract source-code-as-data, e.g. fixture
// loaders that read typescript-eslint's test files.
func (n *Node) LiteralText() string {
	if n == nil || n.inner == nil {
		return ""
	}
	return n.inner.Text()
}

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

// ResolvedSignature returns the signature the type checker resolved
// for a call expression. Returns nil for non-call nodes or when
// resolution fails. Useful when a rule needs the callee's *declared*
// return type rather than the call's contextually-narrowed type
// (the two diverge when a callback context narrows the return).
func (c *Checker) ResolvedSignature(call *Node) *Signature {
	if call == nil || call.inner == nil {
		return nil
	}
	if !ast.IsCallExpression(call.inner) {
		return nil
	}
	sig := c.inner.GetResolvedSignature(call.inner)
	if sig == nil {
		return nil
	}
	return &Signature{inner: sig, checker: c.inner}
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

// CalleeExpression returns the callee of a CallExpression (the
// expression in the position of `f` in `f(args)`). Nil for non-calls.
func (n *Node) CalleeExpression() *Node {
	if n == nil || n.inner == nil || !ast.IsCallExpression(n.inner) {
		return nil
	}
	return &Node{inner: n.inner.Expression()}
}

// PropertyAccessName returns the right-hand identifier text of a
// PropertyAccessExpression (the `b` in `a.b`). Empty for other nodes.
func (n *Node) PropertyAccessName() string {
	if n == nil || n.inner == nil || !ast.IsPropertyAccessExpression(n.inner) {
		return ""
	}
	name := n.inner.AsPropertyAccessExpression().Name()
	if name == nil {
		return ""
	}
	return name.Text()
}

// PropertyAccessReceiver returns the left-hand expression of a
// PropertyAccessExpression (the `a` in `a.b`). Nil for other nodes.
func (n *Node) PropertyAccessReceiver() *Node {
	if n == nil || n.inner == nil || !ast.IsPropertyAccessExpression(n.inner) {
		return nil
	}
	expr := n.inner.AsPropertyAccessExpression().Expression
	if expr == nil {
		return nil
	}
	return &Node{inner: expr}
}

// BinaryOperatorKind returns the AST Kind of the operator token of a
// BinaryExpression (e.g., KindEqualsToken for `=`, KindCommaToken for
// `,`). Returns 0 for non-binary nodes.
func (n *Node) BinaryOperatorKind() Kind {
	if n == nil || n.inner == nil || !ast.IsBinaryExpression(n.inner) {
		return 0
	}
	tok := n.inner.AsBinaryExpression().OperatorToken
	if tok == nil {
		return 0
	}
	return Kind(tok.Kind)
}

// BinaryLeft returns the left operand of a BinaryExpression, or nil.
func (n *Node) BinaryLeft() *Node {
	if n == nil || n.inner == nil || !ast.IsBinaryExpression(n.inner) {
		return nil
	}
	left := n.inner.AsBinaryExpression().Left
	if left == nil {
		return nil
	}
	return &Node{inner: left}
}

// IfCondition returns the condition expression of an IfStatement.
func (n *Node) IfCondition() *Node {
	if n == nil || n.inner == nil || n.inner.Kind != ast.KindIfStatement {
		return nil
	}
	expr := n.inner.AsIfStatement().Expression
	if expr == nil {
		return nil
	}
	return &Node{inner: expr}
}

// WhileCondition returns the condition expression of a WhileStatement
// or DoStatement.
func (n *Node) WhileCondition() *Node {
	if n == nil || n.inner == nil {
		return nil
	}
	switch n.inner.Kind {
	case ast.KindWhileStatement:
		expr := n.inner.AsWhileStatement().Expression
		if expr == nil {
			return nil
		}
		return &Node{inner: expr}
	case ast.KindDoStatement:
		expr := n.inner.AsDoStatement().Expression
		if expr == nil {
			return nil
		}
		return &Node{inner: expr}
	}
	return nil
}

// ForInOrOfExpression returns the iteration expression of a
// ForInStatement or ForOfStatement (the `xs` in `for (k in xs)` or
// `for (k of xs)`). Nil for other kinds.
func (n *Node) ForInOrOfExpression() *Node {
	if n == nil || n.inner == nil {
		return nil
	}
	switch n.inner.Kind {
	case ast.KindForInStatement, ast.KindForOfStatement:
		expr := n.inner.AsForInOrOfStatement().Expression
		if expr == nil {
			return nil
		}
		return &Node{inner: expr}
	}
	return nil
}

// ForStatementCondition returns the condition (middle) expression of
// a ForStatement, or nil if the for has no condition.
func (n *Node) ForStatementCondition() *Node {
	if n == nil || n.inner == nil || n.inner.Kind != ast.KindForStatement {
		return nil
	}
	expr := n.inner.AsForStatement().Condition
	if expr == nil {
		return nil
	}
	return &Node{inner: expr}
}

// ConditionalCondition returns the condition (test) of a
// ConditionalExpression `cond ? then : else`.
func (n *Node) ConditionalCondition() *Node {
	if n == nil || n.inner == nil || n.inner.Kind != ast.KindConditionalExpression {
		return nil
	}
	expr := n.inner.AsConditionalExpression().Condition
	if expr == nil {
		return nil
	}
	return &Node{inner: expr}
}

// PrefixUnaryOperator returns the operator string of a
// PrefixUnaryExpression (e.g. "!", "-", "+", "++"). Empty for other
// kinds.
func (n *Node) PrefixUnaryOperator() string {
	if n == nil || n.inner == nil || n.inner.Kind != ast.KindPrefixUnaryExpression {
		return ""
	}
	switch n.inner.AsPrefixUnaryExpression().Operator {
	case ast.KindExclamationToken:
		return "!"
	case ast.KindMinusToken:
		return "-"
	case ast.KindPlusToken:
		return "+"
	case ast.KindPlusPlusToken:
		return "++"
	case ast.KindMinusMinusToken:
		return "--"
	case ast.KindTildeToken:
		return "~"
	}
	return ""
}

// ConditionalBranches returns the (whenTrue, whenFalse) branches of a
// ConditionalExpression. Both nil for non-conditional nodes.
func (n *Node) ConditionalBranches() (whenTrue, whenFalse *Node) {
	if n == nil || n.inner == nil || n.inner.Kind != ast.KindConditionalExpression {
		return nil, nil
	}
	c := n.inner.AsConditionalExpression()
	if c.WhenTrue != nil {
		whenTrue = &Node{inner: c.WhenTrue}
	}
	if c.WhenFalse != nil {
		whenFalse = &Node{inner: c.WhenFalse}
	}
	return
}

// BinaryRight returns the right operand of a BinaryExpression, or nil.
func (n *Node) BinaryRight() *Node {
	if n == nil || n.inner == nil || !ast.IsBinaryExpression(n.inner) {
		return nil
	}
	right := n.inner.AsBinaryExpression().Right
	if right == nil {
		return nil
	}
	return &Node{inner: right}
}

// ObjectProperties returns the property nodes of an ObjectLiteralExpression
// in source order (typically PropertyAssignment nodes). Nil otherwise.
func (n *Node) ObjectProperties() []*Node {
	if n == nil || n.inner == nil || !ast.IsObjectLiteralExpression(n.inner) {
		return nil
	}
	props := n.inner.AsObjectLiteralExpression().Properties
	if props == nil {
		return nil
	}
	out := make([]*Node, 0, len(props.Nodes))
	for _, p := range props.Nodes {
		out = append(out, &Node{inner: p})
	}
	return out
}

// ArrayElements returns the element nodes of an ArrayLiteralExpression
// in source order. Nil for non-array nodes.
func (n *Node) ArrayElements() []*Node {
	if n == nil || n.inner == nil || !ast.IsArrayLiteralExpression(n.inner) {
		return nil
	}
	elems := n.inner.AsArrayLiteralExpression().Elements
	if elems == nil {
		return nil
	}
	out := make([]*Node, 0, len(elems.Nodes))
	for _, e := range elems.Nodes {
		out = append(out, &Node{inner: e})
	}
	return out
}

// PropertyName returns the textual key of a PropertyAssignment
// (the `code` in `{ code: '...' }`). Empty for other nodes.
func (n *Node) PropertyName() string {
	if n == nil || n.inner == nil || !ast.IsPropertyAssignment(n.inner) {
		return ""
	}
	name := n.inner.AsPropertyAssignment().Name()
	if name == nil {
		return ""
	}
	return name.Text()
}

// PropertyInitializer returns the value expression of a PropertyAssignment
// (the `'...'` in `{ code: '...' }`). Nil for other nodes.
func (n *Node) PropertyInitializer() *Node {
	if n == nil || n.inner == nil || !ast.IsPropertyAssignment(n.inner) {
		return nil
	}
	init := n.inner.AsPropertyAssignment().Initializer
	if init == nil {
		return nil
	}
	return &Node{inner: init}
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

// IsEnumLike reports whether the type is an enum (or enum-literal).
func (t *Type) IsEnumLike() bool {
	if t == nil || t.inner == nil {
		return false
	}
	return t.inner.Flags()&(checker.TypeFlagsEnum|checker.TypeFlagsEnumLiteral) != 0
}

// IsNever reports whether the type is `never` (the empty bottom type).
func (t *Type) IsNever() bool {
	if t == nil || t.inner == nil {
		return false
	}
	return t.inner.Flags()&checker.TypeFlagsNever != 0
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

// IsPromise reports whether the type is the global Promise (or a generic
// Promise<T>) by branded symbol name. Recurses through unions,
// intersections, and base-class hierarchies so subclasses of Promise
// (e.g., `class CanThen extends Promise<T>`) and intersections like
// `Promise<T> & {...}` are matched. Custom thenables that don't share
// an ancestor with `Promise` are not — that's the
// `checkThenables: false` default of typescript-eslint's rule.
func (t *Type) IsPromise() bool {
	if t == nil || t.inner == nil {
		return false
	}
	return t.isPromiseDeep(make(map[*checker.Type]struct{}))
}

func (t *Type) isPromiseDeep(seen map[*checker.Type]struct{}) bool {
	if t == nil || t.inner == nil {
		return false
	}
	if _, ok := seen[t.inner]; ok {
		return false
	}
	seen[t.inner] = struct{}{}
	if t.IsUnion() || t.IsIntersection() {
		for _, m := range t.unionOrIntersectionMembers() {
			if m.isPromiseDeep(seen) {
				return true
			}
		}
		return false
	}
	sym := t.inner.Symbol()
	if sym != nil && sym.Name == "Promise" {
		return true
	}
	// GetBaseTypes is only safe when the type's data is an
	// InterfaceType (declared classes/interfaces, not generic
	// instantiations or anonymous types). For everything else upstream
	// panics. We restrict to symbols declared as a class — the case
	// we care about is `class MyPromise extends Promise<T>`.
	if sym == nil || sym.Flags&ast.SymbolFlagsClass == 0 {
		return false
	}
	bases := safeGetBaseTypes(t.checker, t.inner)
	if len(bases) == 0 {
		// Generic class instantiations (e.g. `MyPromise<number>` for
		// `class MyPromise<T> extends Promise<T> {}`) are
		// TypeReferences and have no resolvable base types directly —
		// the bases live on the originating generic. Try the symbol's
		// declared base via its declarations.
		bases = baseTypesFromClassDeclarations(t.checker, sym)
	}
	for _, base := range bases {
		bt := &Type{inner: base, checker: t.checker}
		if bt.isPromiseDeep(seen) {
			return true
		}
	}
	return false
}

// baseTypesFromClassDeclarations resolves base types by reading the
// `extends` clause from each class declaration tied to the symbol.
// This works for generic-class instantiations where GetBaseTypes
// returns nothing because the underlying TypeReference has no
// InterfaceType data.
func baseTypesFromClassDeclarations(c *checker.Checker, sym *ast.Symbol) []*checker.Type {
	if sym == nil {
		return nil
	}
	var out []*checker.Type
	for _, decl := range sym.Declarations {
		if !ast.IsClassDeclaration(decl) && !ast.IsClassExpression(decl) {
			continue
		}
		clauses := decl.ClassLikeData().HeritageClauses
		if clauses == nil {
			continue
		}
		for _, clause := range clauses.Nodes {
			if clause.Kind != ast.KindHeritageClause || clause.AsHeritageClause().Token != ast.KindExtendsKeyword {
				continue
			}
			types := clause.AsHeritageClause().Types
			if types == nil {
				continue
			}
			for _, t := range types.Nodes {
				bt := c.GetTypeFromTypeNode(t)
				if bt != nil {
					out = append(out, bt)
				}
			}
		}
	}
	return out
}

// safeGetBaseTypes wraps Checker.GetBaseTypes with a panic recovery.
// Upstream's getBaseTypes assumes the type's data is an InterfaceType
// and dereferences without checking; for some object types that
// assumption doesn't hold and the call panics. We treat a panic as
// "no base types" to keep callers from having to encode the upstream's
// internal invariants.
func safeGetBaseTypes(c *checker.Checker, t *checker.Type) (out []*checker.Type) {
	defer func() { _ = recover() }()
	return c.GetBaseTypes(t)
}

// SymbolName returns the name of the symbol the type refers to (e.g.
// "Promise" for `Promise<T>`, "Array" for `Array<T>`, "MyClass" for a
// class instance type). Empty for anonymous or symbol-less types.
func (t *Type) SymbolName() string {
	if t == nil || t.inner == nil {
		return ""
	}
	sym := t.inner.Symbol()
	if sym == nil {
		return ""
	}
	return sym.Name
}

// AliasSymbolName returns the name of the type alias this type was
// instantiated through, if any. For `type Foo = Promise<X> & { ... }`,
// references to `Foo` produce a type whose AliasSymbolName is "Foo"
// even though the underlying SymbolName might be "Promise" or empty.
// Empty for types not introduced via a type alias.
func (t *Type) AliasSymbolName() string {
	if t == nil || t.inner == nil {
		return ""
	}
	sym := t.inner.AliasSymbol()
	if sym == nil {
		return ""
	}
	return sym.Name
}

// IsIntersection reports whether the type is an intersection (T & U).
func (t *Type) IsIntersection() bool {
	if t == nil || t.inner == nil {
		return false
	}
	return t.inner.IsIntersection()
}

// unionOrIntersectionMembers returns the constituent types of a union
// or intersection. Empty for other types.
func (t *Type) unionOrIntersectionMembers() []*Type {
	if t == nil || t.inner == nil {
		return nil
	}
	if !t.IsUnion() && !t.IsIntersection() {
		return nil
	}
	parts := t.inner.Types()
	out := make([]*Type, 0, len(parts))
	for _, p := range parts {
		out = append(out, &Type{inner: p, checker: t.checker})
	}
	return out
}

// IntersectionMembers returns the constituent types of an intersection,
// or a single-element slice for non-intersection types.
func (t *Type) IntersectionMembers() []*Type {
	if t == nil || t.inner == nil {
		return nil
	}
	if !t.IsIntersection() {
		return []*Type{t}
	}
	return t.unionOrIntersectionMembers()
}

// BaseTypeNames returns the symbol names of every base type the type
// inherits from, walking class/interface heritage clauses. Empty for
// types without inheritance information. Used by allow-list checks
// that want to match by ancestor name (e.g. "any subtype of Error").
func (t *Type) BaseTypeNames() []string {
	if t == nil || t.inner == nil {
		return nil
	}
	sym := t.inner.Symbol()
	if sym == nil {
		return nil
	}
	out := []string{}
	seen := map[string]struct{}{}
	var walk func(s *ast.Symbol)
	walk = func(s *ast.Symbol) {
		if s == nil {
			return
		}
		for _, decl := range s.Declarations {
			if !ast.IsClassDeclaration(decl) && !ast.IsClassExpression(decl) &&
				!ast.IsInterfaceDeclaration(decl) {
				continue
			}
			var clauses *ast.NodeList
			switch {
			case ast.IsInterfaceDeclaration(decl):
				clauses = decl.AsInterfaceDeclaration().HeritageClauses
			default:
				clauses = decl.ClassLikeData().HeritageClauses
			}
			if clauses == nil {
				continue
			}
			for _, clause := range clauses.Nodes {
				if clause.Kind != ast.KindHeritageClause {
					continue
				}
				h := clause.AsHeritageClause()
				if h.Token != ast.KindExtendsKeyword && h.Token != ast.KindImplementsKeyword {
					continue
				}
				if h.Types == nil {
					continue
				}
				for _, typeNode := range h.Types.Nodes {
					bt := t.checker.GetTypeFromTypeNode(typeNode)
					if bt == nil || bt.Symbol() == nil {
						continue
					}
					name := bt.Symbol().Name
					if name == "" {
						continue
					}
					if _, dup := seen[name]; dup {
						continue
					}
					seen[name] = struct{}{}
					out = append(out, name)
					walk(bt.Symbol())
				}
			}
		}
	}
	walk(sym)
	return out
}

// BaseConstraint returns the base constraint of a generic type
// parameter (the `T` in `<T extends X>` resolves to `X`). Nil for
// non-generic types or types without a constraint.
func (t *Type) BaseConstraint() *Type {
	if t == nil || t.inner == nil {
		return nil
	}
	c := t.checker.GetBaseConstraintOfType(t.inner)
	if c == nil {
		return nil
	}
	return &Type{inner: c, checker: t.checker}
}

// IsTupleType reports whether the type is a tuple. Tuples are ordered,
// fixed-length array types whose elements may have distinct types.
func (t *Type) IsTupleType() bool {
	if t == nil || t.inner == nil {
		return false
	}
	return checker.IsTupleType(t.inner)
}

// IsArrayLikeType reports whether the type is array-like — Array<T>,
// readonly array, or any type with a numeric index signature and
// `length`. Tuples are also array-like.
func (t *Type) IsArrayLikeType() bool {
	if t == nil || t.inner == nil {
		return false
	}
	return t.checker.IsArrayLikeType(t.inner)
}

// TypeArguments returns the type arguments of a generic type reference
// (e.g., the element types of a tuple, or the `T` in `Array<T>`). Empty
// for non-generic types.
func (t *Type) TypeArguments() []*Type {
	if t == nil || t.inner == nil {
		return nil
	}
	args := t.checker.GetTypeArguments(t.inner)
	out := make([]*Type, 0, len(args))
	for _, a := range args {
		out = append(out, &Type{inner: a, checker: t.checker})
	}
	return out
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
	// Well-known JS built-ins whose toString is defined in the lib but
	// whose declaration tsgo's checker reports as living on Object —
	// kept as an explicit list because the type-checker view doesn't
	// resolve their lib.es5.d.ts override consistently.
	switch t.SymbolName() {
	case "RegExp", "Date", "Symbol", "Map", "Set", "WeakMap", "WeakSet",
		"Error", "TypeError", "RangeError", "SyntaxError", "ReferenceError",
		"URL", "URLSearchParams", "ArrayBuffer", "SharedArrayBuffer",
		"Int8Array", "Uint8Array", "Uint8ClampedArray",
		"Int16Array", "Uint16Array", "Int32Array", "Uint32Array",
		"Float32Array", "Float64Array", "BigInt64Array", "BigUint64Array",
		"BigInt", "Promise":
		return true
	}
	if sym := t.checker.GetPropertyOfType(t.inner, "toString"); sym != nil {
		if symbolDeclaresToStringOutsideObject(sym) {
			return true
		}
	}
	if sym := t.checker.GetPropertyOfType(t.inner, "toLocaleString"); sym != nil {
		if symbolDeclaresToStringOutsideObject(sym) {
			return true
		}
	}
	if sym := t.checker.GetPropertyOfType(t.inner, "valueOf"); sym != nil {
		if symbolDeclaresToStringOutsideObject(sym) {
			return true
		}
	}
	// A custom Symbol.toPrimitive method also produces meaningful
	// string conversion. The property name in the symbol table is
	// "__@toPrimitive@..." (the well-known symbol gets a synthetic
	// name); look it up by walking apparent properties.
	for _, p := range t.checker.GetApparentProperties(t.inner) {
		if len(p.Name) > 13 && p.Name[:13] == "__@toPrimitive" {
			return true
		}
	}
	return false
}

// debugToStringDeclarations returns a description of where the type's
// toString symbol is declared. Used by external tooling for diagnosing
// the no-base-to-string rule's behavior.
func (t *Type) DebugToStringDeclarations() string {
	if t == nil || t.inner == nil {
		return "<nil type>"
	}
	sym := t.checker.GetPropertyOfType(t.inner, "toString")
	if sym == nil {
		return "<no toString symbol>"
	}
	out := fmt.Sprintf("symbol %q parent=", sym.Name)
	if sym.Parent != nil {
		out += fmt.Sprintf("%q", sym.Parent.Name)
	} else {
		out += "<nil>"
	}
	out += fmt.Sprintf(" decls=%d", len(sym.Declarations))
	for _, d := range sym.Declarations {
		out += fmt.Sprintf(" container=%q", containingInterfaceOrClassName(d))
	}
	return out
}

// symbolDeclaresToStringOutsideObject walks each declaration of the
// symbol and checks the containing interface/class declaration name.
// If any declaration lives in something other than the global Object
// interface, we treat the type as having a meaningful toString.
func symbolDeclaresToStringOutsideObject(sym *ast.Symbol) bool {
	for _, decl := range sym.Declarations {
		if container := containingInterfaceOrClassName(decl); container != "" && container != "Object" {
			return true
		}
	}
	// Some declarations sit directly on a TypeLiteral/ObjectLiteral with
	// no named interface — treat those as meaningful (the user wrote
	// them, so they intend their toString to mean something).
	for _, decl := range sym.Declarations {
		if containingInterfaceOrClassName(decl) == "" {
			return true
		}
	}
	return false
}

func containingInterfaceOrClassName(n *ast.Node) string {
	for cur := n; cur != nil; cur = cur.Parent {
		switch cur.Kind {
		case ast.KindInterfaceDeclaration:
			if name := cur.AsInterfaceDeclaration().Name(); name != nil {
				return name.Text()
			}
			return ""
		case ast.KindClassDeclaration:
			if name := cur.AsClassDeclaration().Name(); name != nil {
				return name.Text()
			}
			return ""
		}
	}
	return ""
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

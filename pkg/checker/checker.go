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
	"strings"

	"github.com/microsoft/typescript-go/internal/ast"
	"github.com/microsoft/typescript-go/internal/bundled"
	"github.com/microsoft/typescript-go/internal/checker"
	"github.com/microsoft/typescript-go/internal/compiler"
	"github.com/microsoft/typescript-go/internal/core"
	"github.com/microsoft/typescript-go/internal/jsnum"
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
// HasStrictNullChecks reports whether the program was compiled with
// strict null checks enabled (via `"strict": true` or
// `"strictNullChecks": true`).
func (p *Program) HasStrictNullChecks() bool {
	if p == nil || p.inner == nil {
		return false
	}
	opts := p.inner.Options()
	if opts == nil {
		return false
	}
	if opts.StrictNullChecks == core.TSTrue {
		return true
	}
	if opts.StrictNullChecks == core.TSFalse {
		return false
	}
	return opts.Strict == core.TSTrue
}

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

// SourceText returns the literal source text spanning the node, taken
// from the owning source file. Useful for structural equivalence
// comparisons of expressions whose AST shapes match but whose source
// positions differ.
func (n *Node) SourceText() string {
	if n == nil || n.inner == nil {
		return ""
	}
	sf := ast.GetSourceFileOfNode(n.inner)
	if sf == nil {
		return ""
	}
	text := sf.Text()
	startPos := scanner.GetTokenPosOfNode(n.inner, sf, false)
	end := n.inner.End()
	if startPos < 0 || end < 0 || startPos > end || end > len(text) {
		return ""
	}
	return text[startPos:end]
}

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
	KindTemplateHead             = Kind(ast.KindTemplateHead)
	KindTemplateMiddle           = Kind(ast.KindTemplateMiddle)
	KindTemplateTail             = Kind(ast.KindTemplateTail)
	KindTemplateLiteralType      = Kind(ast.KindTemplateLiteralType)
	KindTemplateLiteralTypeSpan  = Kind(ast.KindTemplateLiteralTypeSpan)
	KindUndefinedKeyword         = Kind(ast.KindUndefinedKeyword)
	KindJsxAttribute             = Kind(ast.KindJsxAttribute)
	KindJsxAttributes            = Kind(ast.KindJsxAttributes)
	KindJsxExpression            = Kind(ast.KindJsxExpression)
	KindArrayBindingPattern      = Kind(ast.KindArrayBindingPattern)
	KindObjectBindingPattern     = Kind(ast.KindObjectBindingPattern)
	KindLiteralType              = Kind(ast.KindLiteralType)
	KindVariableStatement        = Kind(ast.KindVariableStatement)
	KindTypeAliasDeclaration     = Kind(ast.KindTypeAliasDeclaration)
	KindModuleBlock              = Kind(ast.KindModuleBlock)
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
	KindShorthandPropertyAssignment = Kind(ast.KindShorthandPropertyAssignment)
	KindEnumDeclaration             = Kind(ast.KindEnumDeclaration)
	KindEnumMember                  = Kind(ast.KindEnumMember)
	KindPropertyDeclaration         = Kind(ast.KindPropertyDeclaration)
	KindBigIntLiteral               = Kind(ast.KindBigIntLiteral)
	KindUnionType                   = Kind(ast.KindUnionType)
	KindIntersectionType            = Kind(ast.KindIntersectionType)
	KindFunctionType                = Kind(ast.KindFunctionType)
	KindCallSignature               = Kind(ast.KindCallSignature)
	KindMethodSignature             = Kind(ast.KindMethodSignature)
	KindConstructorType             = Kind(ast.KindConstructorType)
	KindParenthesizedType           = Kind(ast.KindParenthesizedType)
	KindThisKeyword                 = Kind(ast.KindThisKeyword)
	KindIndexSignature              = Kind(ast.KindIndexSignature)
	KindConstructSignature          = Kind(ast.KindConstructSignature)
	KindAnyKeyword                  = Kind(ast.KindAnyKeyword)
	KindUnknownKeyword              = Kind(ast.KindUnknownKeyword)
	KindBindingElement              = Kind(ast.KindBindingElement)
	KindDefaultClause               = Kind(ast.KindDefaultClause)
	KindTryStatement                = Kind(ast.KindTryStatement)
	KindCatchClause                 = Kind(ast.KindCatchClause)
	KindModuleDeclaration           = Kind(ast.KindModuleDeclaration)
	KindQualifiedName               = Kind(ast.KindQualifiedName)
	KindConstructor                 = Kind(ast.KindConstructor)
	KindTypeReference               = Kind(ast.KindTypeReference)
	KindPostfixUnaryExpression      = Kind(ast.KindPostfixUnaryExpression)
	KindBarBarEqualsToken           = Kind(ast.KindBarBarEqualsToken)
	KindMinusEqualsToken            = Kind(ast.KindMinusEqualsToken)
	KindAsteriskEqualsToken         = Kind(ast.KindAsteriskEqualsToken)
	KindAsteriskAsteriskEqualsToken = Kind(ast.KindAsteriskAsteriskEqualsToken)
	KindSlashEqualsToken            = Kind(ast.KindSlashEqualsToken)
	KindPercentEqualsToken          = Kind(ast.KindPercentEqualsToken)
	KindAmpersandEqualsToken        = Kind(ast.KindAmpersandEqualsToken)
	KindBarEqualsToken              = Kind(ast.KindBarEqualsToken)
	KindCaretEqualsToken            = Kind(ast.KindCaretEqualsToken)
	KindLessThanLessThanEqualsToken = Kind(ast.KindLessThanLessThanEqualsToken)
	KindGreaterThanGreaterThanEqualsToken            = Kind(ast.KindGreaterThanGreaterThanEqualsToken)
	KindGreaterThanGreaterThanGreaterThanEqualsToken = Kind(ast.KindGreaterThanGreaterThanGreaterThanEqualsToken)
	KindAmpersandAmpersandEqualsToken = Kind(ast.KindAmpersandAmpersandEqualsToken)
	KindQuestionQuestionEqualsToken = Kind(ast.KindQuestionQuestionEqualsToken)
	KindVariableDeclaration      = Kind(ast.KindVariableDeclaration)
	KindStringLiteral            = Kind(ast.KindStringLiteral)
	KindParenthesizedExpression  = Kind(ast.KindParenthesizedExpression)
	KindVoidExpression           = Kind(ast.KindVoidExpression)
	KindObjectLiteralExpression  = Kind(ast.KindObjectLiteralExpression)
	KindArrayLiteralExpression   = Kind(ast.KindArrayLiteralExpression)
	KindOmittedExpression        = Kind(ast.KindOmittedExpression)
	KindPrivateIdentifier        = Kind(ast.KindPrivateIdentifier)
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
	KindSatisfiesExpression      = Kind(ast.KindSatisfiesExpression)
	KindTypeAssertionExpression  = Kind(ast.KindTypeAssertionExpression)
	KindNonNullExpression        = Kind(ast.KindNonNullExpression)
	KindDeleteExpression         = Kind(ast.KindDeleteExpression)
	KindTypeOfExpression         = Kind(ast.KindTypeOfExpression)
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
	KindRegularExpressionLiteral = Kind(ast.KindRegularExpressionLiteral)
	KindBlock                    = Kind(ast.KindBlock)
	KindYieldExpression          = Kind(ast.KindYieldExpression)
	KindExpressionWithTypeArguments = Kind(ast.KindExpressionWithTypeArguments)
	KindHeritageClause           = Kind(ast.KindHeritageClause)
	KindExtendsKeyword           = Kind(ast.KindExtendsKeyword)
	KindImplementsKeyword        = Kind(ast.KindImplementsKeyword)
	KindJsxOpeningElement        = Kind(ast.KindJsxOpeningElement)
	KindJsxSelfClosingElement    = Kind(ast.KindJsxSelfClosingElement)
	KindImportKeyword            = Kind(ast.KindImportKeyword)
	KindGetAccessor              = Kind(ast.KindGetAccessor)
	KindSetAccessor              = Kind(ast.KindSetAccessor)
	KindInterfaceDeclaration     = Kind(ast.KindInterfaceDeclaration)
	KindClassDeclaration         = Kind(ast.KindClassDeclaration)
	KindClassExpression          = Kind(ast.KindClassExpression)
	KindTypeLiteral              = Kind(ast.KindTypeLiteral)
	KindParameter                = Kind(ast.KindParameter)
	KindVariableDeclarationList  = Kind(ast.KindVariableDeclarationList)
	KindImportDeclaration        = Kind(ast.KindImportDeclaration)
	KindImportClause             = Kind(ast.KindImportClause)
	KindImportSpecifier          = Kind(ast.KindImportSpecifier)
	KindImportEqualsDeclaration  = Kind(ast.KindImportEqualsDeclaration)
	KindNamespaceImport          = Kind(ast.KindNamespaceImport)
	KindNamedImports             = Kind(ast.KindNamedImports)
	KindExportDeclaration        = Kind(ast.KindExportDeclaration)
	KindExportSpecifier          = Kind(ast.KindExportSpecifier)
	KindExportAssignment         = Kind(ast.KindExportAssignment)
	KindNamedExports             = Kind(ast.KindNamedExports)
	KindNamespaceExport          = Kind(ast.KindNamespaceExport)
	KindNamespaceExportDeclaration = Kind(ast.KindNamespaceExportDeclaration)
	KindPropertySignature        = Kind(ast.KindPropertySignature)
	KindJsxIdentifier            = Kind(ast.KindIdentifier) // JSX uses regular identifiers in tsgo
	KindJsxClosingElement        = Kind(ast.KindJsxClosingElement)
	KindSuperKeyword             = Kind(ast.KindSuperKeyword)
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

// ContextualTypeOf returns the contextually-expected type at the
// given expression position (e.g. the property's declared type when n
// is the value in an object literal whose target type is annotated).
// Nil for positions without a contextual type.
func (c *Checker) ContextualTypeOf(n *Node) *Type {
	if n == nil || n.inner == nil || c == nil || c.inner == nil {
		return nil
	}
	t := c.inner.GetContextualType(n.inner, checker.ContextFlagsNone)
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
// HasAsyncModifier reports whether a function-like node has the
// `async` keyword modifier, including async generators that
// IsAsyncFunction excludes.
func HasAsyncModifier(n *Node) bool {
	if n == nil || n.inner == nil {
		return false
	}
	switch n.inner.Kind {
	case ast.KindFunctionDeclaration, ast.KindFunctionExpression,
		ast.KindArrowFunction, ast.KindMethodDeclaration:
		return ast.HasSyntacticModifier(n.inner, ast.ModifierFlagsAsync)
	}
	return false
}

// HasDeclareModifier reports whether the node was written with the
// `declare` keyword (e.g. `declare class`, `declare const`,
// `declare function`).
func (n *Node) HasDeclareModifier() bool {
	if n == nil || n.inner == nil {
		return false
	}
	return ast.HasSyntacticModifier(n.inner, ast.ModifierFlagsAmbient)
}

// HasAbstractModifier reports whether a method-like has the `abstract`
// keyword. Abstract methods carry only a signature and cannot be
// declared async.
func (n *Node) HasAbstractModifier() bool {
	if n == nil || n.inner == nil {
		return false
	}
	return ast.HasSyntacticModifier(n.inner, ast.ModifierFlagsAbstract)
}

// HasPrivateModifier reports whether n has the `private` keyword.
func (n *Node) HasPrivateModifier() bool {
	if n == nil || n.inner == nil {
		return false
	}
	return ast.HasSyntacticModifier(n.inner, ast.ModifierFlagsPrivate)
}

// HasAccessorModifier reports whether n has the `accessor` keyword
// (auto-accessor field, with implicit getter/setter pair). Such
// fields cannot be declared `readonly` because the setter would
// disappear silently.
func (n *Node) HasAccessorModifier() bool {
	if n == nil || n.inner == nil {
		return false
	}
	return ast.HasSyntacticModifier(n.inner, ast.ModifierFlagsAccessor)
}

// IsOptionalParameter reports whether n is a parameter declared with
// a trailing `?` token (e.g. `(x?: T) => void`). When true, the
// parameter's runtime type implicitly includes `undefined`.
func (n *Node) IsOptionalParameter() bool {
	if n == nil || n.inner == nil || n.inner.Kind != ast.KindParameter {
		return false
	}
	pd := n.inner.AsParameterDeclaration()
	if pd == nil {
		return false
	}
	return pd.QuestionToken != nil
}

// IsRestParameter reports whether n is a parameter declared with a
// leading `...` token (and is therefore typed as the rest tuple/array
// rather than a single argument value).
func (n *Node) IsRestParameter() bool {
	if n == nil || n.inner == nil || n.inner.Kind != ast.KindParameter {
		return false
	}
	pd := n.inner.AsParameterDeclaration()
	if pd == nil {
		return false
	}
	return pd.DotDotDotToken != nil
}

// HasProtectedModifier reports whether n has the `protected` keyword.
func (n *Node) HasProtectedModifier() bool {
	if n == nil || n.inner == nil {
		return false
	}
	return ast.HasSyntacticModifier(n.inner, ast.ModifierFlagsProtected)
}

// HasReadonlyModifier reports whether n has the `readonly` keyword.
func (n *Node) HasReadonlyModifier() bool {
	if n == nil || n.inner == nil {
		return false
	}
	return ast.HasSyntacticModifier(n.inner, ast.ModifierFlagsReadonly)
}

// HasStaticModifier reports whether n has the `static` keyword.
func (n *Node) HasStaticModifier() bool {
	if n == nil || n.inner == nil {
		return false
	}
	return ast.HasSyntacticModifier(n.inner, ast.ModifierFlagsStatic)
}

func IsAsyncFunction(n *Node) bool {
	if n == nil || n.inner == nil {
		return false
	}
	return ast.IsAsyncFunction(n.inner)
}

// CallArguments returns the argument expressions of a CallExpression
// or NewExpression, in source order. Returns nil for nodes of other
// kinds.
func (n *Node) CallArguments() []*Node {
	if n == nil || n.inner == nil {
		return nil
	}
	if !ast.IsCallExpression(n.inner) && !ast.IsNewExpression(n.inner) {
		return nil
	}
	args := n.inner.Arguments()
	out := make([]*Node, 0, len(args))
	for _, a := range args {
		out = append(out, &Node{inner: a})
	}
	return out
}

// CalleeExpression returns the callee of a CallExpression or
// NewExpression (the expression in the position of `f` in `f(args)`
// or `new C(args)`). Nil for other node kinds.
func (n *Node) CalleeExpression() *Node {
	if n == nil || n.inner == nil {
		return nil
	}
	if !ast.IsCallExpression(n.inner) && !ast.IsNewExpression(n.inner) {
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

// IfThen returns the then-branch statement of an IfStatement.
func (n *Node) IfThen() *Node {
	if n == nil || n.inner == nil || n.inner.Kind != ast.KindIfStatement {
		return nil
	}
	stmt := n.inner.AsIfStatement().ThenStatement
	if stmt == nil {
		return nil
	}
	return &Node{inner: stmt}
}

// IfElse returns the else-branch statement of an IfStatement, or nil
// when the if has no else.
func (n *Node) IfElse() *Node {
	if n == nil || n.inner == nil || n.inner.Kind != ast.KindIfStatement {
		return nil
	}
	stmt := n.inner.AsIfStatement().ElseStatement
	if stmt == nil {
		return nil
	}
	return &Node{inner: stmt}
}

// BlockStatements returns the statements of a Block.
func (n *Node) BlockStatements() []*Node {
	if n == nil || n.inner == nil || n.inner.Kind != ast.KindBlock {
		return nil
	}
	stmts := n.inner.AsBlock().Statements
	if stmts == nil {
		return nil
	}
	out := make([]*Node, 0, len(stmts.Nodes))
	for _, s := range stmts.Nodes {
		out = append(out, &Node{inner: s})
	}
	return out
}

// ExpressionStatementExpression returns the expression of an
// ExpressionStatement.
func (n *Node) ExpressionStatementExpression() *Node {
	if n == nil || n.inner == nil || n.inner.Kind != ast.KindExpressionStatement {
		return nil
	}
	expr := n.inner.AsExpressionStatement().Expression
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

// DeclaredTypeOfIdentifier returns the type of the identifier as
// declared at its declaration site (e.g. for a parameter
// `source: AsyncIterable<X>`, returns the AsyncIterable<X> type even
// if the identifier's apparent type at the use site has narrowed to
// `any`). Nil for unresolved or undeclared identifiers.
func (n *Node) DeclaredTypeOfIdentifier(c *Checker) *Type {
	if n == nil || n.inner == nil || c == nil || c.inner == nil {
		return nil
	}
	if n.inner.Kind != ast.KindIdentifier {
		return nil
	}
	sym := c.inner.GetSymbolAtLocation(n.inner)
	if sym == nil {
		return nil
	}
	for _, decl := range sym.Declarations {
		var typeNode *ast.Node
		switch decl.Kind {
		case ast.KindParameter:
			typeNode = decl.AsParameterDeclaration().Type
		case ast.KindVariableDeclaration:
			typeNode = decl.AsVariableDeclaration().Type
		case ast.KindPropertySignature:
			typeNode = decl.AsPropertySignatureDeclaration().Type
		case ast.KindPropertyDeclaration:
			typeNode = decl.AsPropertyDeclaration().Type
		}
		if typeNode == nil {
			continue
		}
		t := c.inner.GetTypeFromTypeNode(typeNode)
		if t != nil {
			return &Type{inner: t, checker: c.inner}
		}
	}
	return nil
}

// ElementAccessReceiver and ElementAccessIndex return the two
// expressions of an ElementAccessExpression (`obj[idx]`). Both nil
// for non-element-access nodes.
func (n *Node) ElementAccessReceiver() *Node {
	if n == nil || n.inner == nil || n.inner.Kind != ast.KindElementAccessExpression {
		return nil
	}
	expr := n.inner.AsElementAccessExpression().Expression
	if expr == nil {
		return nil
	}
	return &Node{inner: expr}
}

func (n *Node) ElementAccessIndex() *Node {
	if n == nil || n.inner == nil || n.inner.Kind != ast.KindElementAccessExpression {
		return nil
	}
	idx := n.inner.AsElementAccessExpression().ArgumentExpression
	if idx == nil {
		return nil
	}
	return &Node{inner: idx}
}

// FunctionReturnTypeAnnotation returns the explicit return-type
// annotation of a function-like node, or nil for inferred returns or
// non-function nodes. The returned node is a TypeNode — convert to a
// Type via Checker.TypeFromTypeNode.
func (n *Node) FunctionReturnTypeAnnotation() *Node {
	if n == nil || n.inner == nil {
		return nil
	}
	fn := n.inner.FunctionLikeData()
	if fn == nil || fn.Type == nil {
		return nil
	}
	return &Node{inner: fn.Type}
}

// TypeFromTypeNode resolves a TypeNode to its Type. Used in tandem
// with FunctionReturnTypeAnnotation to read declared return types.
func (c *Checker) TypeFromTypeNode(n *Node) *Type {
	if n == nil || n.inner == nil || c == nil || c.inner == nil {
		return nil
	}
	t := c.inner.GetTypeFromTypeNode(n.inner)
	if t == nil {
		return nil
	}
	return &Type{inner: t, checker: c.inner}
}

// IsYieldDelegate reports whether a YieldExpression is `yield*` (a
// generator-delegate yield).
func (n *Node) IsYieldDelegate() bool {
	if n == nil || n.inner == nil || n.inner.Kind != ast.KindYieldExpression {
		return false
	}
	return n.inner.AsYieldExpression().AsteriskToken != nil
}

// HasAwaitModifier reports whether a ForOfStatement has the `await`
// keyword modifier (i.e. is `for await (... of ...)`). False for other
// kinds.
func (n *Node) HasAwaitModifier() bool {
	if n == nil || n.inner == nil || n.inner.Kind != ast.KindForOfStatement {
		return false
	}
	return n.inner.AsForInOrOfStatement().AwaitModifier != nil
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

// BindingElementInitializer returns the default-value expression of a
// BindingElement (the `''` in `{ foo = '' }`). Nil for elements with
// no default and non-BindingElement nodes.
func (n *Node) BindingElementInitializer() *Node {
	if n == nil || n.inner == nil || n.inner.Kind != ast.KindBindingElement {
		return nil
	}
	init := n.inner.AsBindingElement().Initializer
	if init == nil {
		return nil
	}
	return &Node{inner: init}
}

// IsOptionalChain reports whether n is part of an optional-chain
// expression (`x?.y`, `x?.[idx]`, `x?.()`, or any link inside a chain
// rooted at one of those).
func (n *Node) IsOptionalChain() bool {
	if n == nil || n.inner == nil {
		return false
	}
	return ast.IsOptionalChain(n.inner)
}

// IsOptionalChainRoot reports whether n is the root link of an
// optional chain — the specific node carrying the `?.` token, as
// opposed to any other link in the chain.
func (n *Node) IsOptionalChainRoot() bool {
	if n == nil || n.inner == nil {
		return false
	}
	return ast.IsOptionalChainRoot(n.inner)
}

// TypeAssertionSource returns the value expression of a
// TypeAssertion (`<T>expr`). Nil for non-type-assertion nodes.
func (n *Node) TypeAssertionSource() *Node {
	if n == nil || n.inner == nil || n.inner.Kind != ast.KindTypeAssertionExpression {
		return nil
	}
	expr := n.inner.AsTypeAssertion().Expression
	if expr == nil {
		return nil
	}
	return &Node{inner: expr}
}

// TypeAssertionTarget returns the type-annotation node of a
// TypeAssertion (`<T>expr`). Nil for non-type-assertion nodes.
func (n *Node) TypeAssertionTarget() *Node {
	if n == nil || n.inner == nil || n.inner.Kind != ast.KindTypeAssertionExpression {
		return nil
	}
	t := n.inner.AsTypeAssertion().Type
	if t == nil {
		return nil
	}
	return &Node{inner: t}
}

// AsExpressionTarget returns the type-annotation node of an
// AsExpression (the `T` in `expr as T`). Nil for non-as nodes.
func (n *Node) AsExpressionTarget() *Node {
	if n == nil || n.inner == nil || n.inner.Kind != ast.KindAsExpression {
		return nil
	}
	t := n.inner.AsAsExpression().Type
	if t == nil {
		return nil
	}
	return &Node{inner: t}
}

// AsExpressionSource returns the value expression of an AsExpression
// (the `expr` in `expr as T`). Nil for non-as nodes.
func (n *Node) AsExpressionSource() *Node {
	if n == nil || n.inner == nil || n.inner.Kind != ast.KindAsExpression {
		return nil
	}
	expr := n.inner.AsAsExpression().Expression
	if expr == nil {
		return nil
	}
	return &Node{inner: expr}
}

// ForOfIterable returns the iterable expression of a ForOfStatement
// (the `xs` in `for (const x of xs)`). Nil for non-for-of nodes.
func (n *Node) ForOfIterable() *Node {
	if n == nil || n.inner == nil || n.inner.Kind != ast.KindForOfStatement {
		return nil
	}
	expr := n.inner.AsForInOrOfStatement().Expression
	if expr == nil {
		return nil
	}
	return &Node{inner: expr}
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

// PrefixUnaryOperand returns the operand of a PrefixUnaryExpression,
// or nil for other kinds.
func (n *Node) PrefixUnaryOperand() *Node {
	if n == nil || n.inner == nil || n.inner.Kind != ast.KindPrefixUnaryExpression {
		return nil
	}
	op := n.inner.AsPrefixUnaryExpression().Operand
	if op == nil {
		return nil
	}
	return &Node{inner: op}
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

// IsAwaitUsingDeclaration reports whether a VariableDeclarationList
// is an `await using` declaration. The wrapper hides the bit-twiddle
// callers would otherwise need to do on combined node flags.
func (n *Node) IsAwaitUsingDeclaration() bool {
	if n == nil || n.inner == nil {
		return false
	}
	return ast.IsVarAwaitUsing(n.inner)
}

// VariableStatementDeclarationList returns the VariableDeclarationList
// child of a VariableStatement (`const x = 1, y = 2` → the list node).
// Nil for other kinds of nodes.
func (n *Node) VariableStatementDeclarationList() *Node {
	if n == nil || n.inner == nil || n.inner.Kind != ast.KindVariableStatement {
		return nil
	}
	dl := n.inner.AsVariableStatement().DeclarationList
	if dl == nil {
		return nil
	}
	return &Node{inner: dl}
}

// EnumMemberInitializer returns the initializer expression of an
// EnumMember (the `'a'` in `Apple = 'a'`). Nil for members with no
// explicit initializer or non-EnumMember nodes.
func (n *Node) EnumMemberInitializer() *Node {
	if n == nil || n.inner == nil || n.inner.Kind != ast.KindEnumMember {
		return nil
	}
	init := n.inner.AsEnumMember().Initializer
	if init == nil {
		return nil
	}
	return &Node{inner: init}
}

// SwitchExpression returns the discriminant expression of a
// SwitchStatement (the `e` in `switch (e) { ... }`). Nil for non-switch
// nodes.
func (n *Node) SwitchExpression() *Node {
	if n == nil || n.inner == nil || n.inner.Kind != ast.KindSwitchStatement {
		return nil
	}
	expr := n.inner.AsSwitchStatement().Expression
	if expr == nil {
		return nil
	}
	return &Node{inner: expr}
}

// CaseExpression returns the case-label expression of a CaseClause
// (the `0` in `case 0:`). Nil for default clauses or non-case nodes.
func (n *Node) CaseExpression() *Node {
	if n == nil || n.inner == nil || n.inner.Kind != ast.KindCaseClause {
		return nil
	}
	expr := n.inner.AsCaseOrDefaultClause().Expression
	if expr == nil {
		return nil
	}
	return &Node{inner: expr}
}

// YieldOperand returns the operand expression of a YieldExpression
// (the `X` in `yield X` or `yield* X`). For `yield;` (no operand),
// returns nil. Skips the `*` token so callers don't have to.
func (n *Node) YieldOperand() *Node {
	if n == nil || n.inner == nil || n.inner.Kind != ast.KindYieldExpression {
		return nil
	}
	expr := n.inner.AsYieldExpression().Expression
	if expr == nil {
		return nil
	}
	return &Node{inner: expr}
}

// FunctionBody returns the body of a function-like node, or nil for
// non-function nodes or function declarations without a body. For
// ArrowFunctions with an expression body, returns the expression node;
// for block-body functions, returns the BlockStatement node.
func (n *Node) FunctionBody() *Node {
	if n == nil || n.inner == nil {
		return nil
	}
	body := n.inner.BodyData()
	if body == nil || body.Body == nil {
		return nil
	}
	return &Node{inner: body.Body}
}

// FunctionReturnType returns the explicit return-type annotation of a
// function-like node (`function f(): T {}` returns the `T` type-node),
// or nil for arrow expressions, declarations without an annotation, or
// non-function nodes.
func (n *Node) FunctionReturnType() *Node {
	if n == nil || n.inner == nil {
		return nil
	}
	switch n.inner.Kind {
	case ast.KindFunctionDeclaration,
		ast.KindFunctionExpression,
		ast.KindArrowFunction,
		ast.KindMethodDeclaration,
		ast.KindMethodSignature,
		ast.KindFunctionType,
		ast.KindConstructorType,
		ast.KindCallSignature,
		ast.KindConstructSignature,
		ast.KindGetAccessor:
	default:
		return nil
	}
	t := n.inner.Type()
	if t == nil {
		return nil
	}
	return &Node{inner: t}
}

// VariableDeclarationType returns the explicit type annotation of a
// VariableDeclaration (the `() => void` in `const f: () => void = ...`),
// or nil for non-VariableDeclaration nodes or untyped declarations.
func (n *Node) VariableDeclarationType() *Node {
	if n == nil || n.inner == nil || n.inner.Kind != ast.KindVariableDeclaration {
		return nil
	}
	t := n.inner.AsVariableDeclaration().Type
	if t == nil {
		return nil
	}
	return &Node{inner: t}
}

// ParameterTypeAnnotation returns the explicit type annotation of a
// Parameter node, or nil for parameters without one.
func (n *Node) ParameterTypeAnnotation() *Node {
	if n == nil || n.inner == nil || n.inner.Kind != ast.KindParameter {
		return nil
	}
	t := n.inner.AsParameterDeclaration().Type
	if t == nil {
		return nil
	}
	return &Node{inner: t}
}

// PropertyDeclarationType returns the explicit type annotation of a
// class PropertyDeclaration, or nil for fields without one.
func (n *Node) PropertyDeclarationType() *Node {
	if n == nil || n.inner == nil || n.inner.Kind != ast.KindPropertyDeclaration {
		return nil
	}
	t := n.inner.AsPropertyDeclaration().Type
	if t == nil {
		return nil
	}
	return &Node{inner: t}
}

// ParameterInitializer returns the default-value expression of a
// Parameter (the `1 as any` in `(a = 1 as any) => {}`). Nil for
// parameters without a default and non-Parameter nodes.
func (n *Node) ParameterInitializer() *Node {
	if n == nil || n.inner == nil || n.inner.Kind != ast.KindParameter {
		return nil
	}
	init := n.inner.AsParameterDeclaration().Initializer
	if init == nil {
		return nil
	}
	return &Node{inner: init}
}

// PropertyDeclarationInitializer returns the initializer expression of
// a class PropertyDeclaration (`a` in `class C { a = 1 }`). Nil for
// fields without initializer or non-PropertyDeclaration nodes.
func (n *Node) PropertyDeclarationInitializer() *Node {
	if n == nil || n.inner == nil || n.inner.Kind != ast.KindPropertyDeclaration {
		return nil
	}
	init := n.inner.AsPropertyDeclaration().Initializer
	if init == nil {
		return nil
	}
	return &Node{inner: init}
}

// VariableDeclarationInitializer returns the initializer expression of
// a VariableDeclaration. Nil if missing or for non-VariableDeclaration
// nodes.
func (n *Node) VariableDeclarationInitializer() *Node {
	if n == nil || n.inner == nil || n.inner.Kind != ast.KindVariableDeclaration {
		return nil
	}
	init := n.inner.AsVariableDeclaration().Initializer
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

// ShorthandAssignmentValueSymbol returns the value-binding symbol for a
// shorthand property assignment identifier (`{ x }` reads the local
// `x`). Returns nil for nodes that aren't shorthand assignment names.
func (c *Checker) ShorthandAssignmentValueSymbol(n *Node) *Symbol {
	if n == nil || n.inner == nil {
		return nil
	}
	s := c.inner.GetShorthandAssignmentValueSymbol(n.inner)
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

// SymbolHasUserDeclaration reports whether the node's resolved symbol
// has any declaration that lives in a non-declaration source file —
// i.e. the user's own .ts source rather than a bundled lib.*.d.ts.
// Used by rules that need to distinguish a global like `String` from
// a user-shadowed redefinition.
func (n *Node) SymbolHasUserDeclaration(c *Checker) bool {
	if n == nil || n.inner == nil || c == nil || c.inner == nil {
		return false
	}
	return symbolHasUserDeclaration(c.inner.GetSymbolAtLocation(n.inner))
}

func symbolHasUserDeclaration(sym *ast.Symbol) bool {
	if sym == nil {
		return false
	}
	for _, decl := range sym.Declarations {
		sf := ast.GetSourceFileOfNode(decl)
		if sf == nil {
			continue
		}
		if !sf.IsDeclarationFile {
			return true
		}
	}
	return false
}

// SymbolDeclarationCount returns the count of declaration sites for
// the resolved symbol. Useful for debugging why a symbol reports as
// global vs user-shadowed.
func (n *Node) SymbolDeclarationCount(c *Checker) int {
	if n == nil || n.inner == nil || c == nil || c.inner == nil {
		return 0
	}
	sym := c.inner.GetSymbolAtLocation(n.inner)
	if sym == nil {
		return 0
	}
	return len(sym.Declarations)
}

// SymbolUserDeclarationCount returns the count of declaration sites
// in user (non-.d.ts) source files.
func (n *Node) SymbolUserDeclarationCount(c *Checker) int {
	if n == nil || n.inner == nil || c == nil || c.inner == nil {
		return 0
	}
	sym := c.inner.GetSymbolAtLocation(n.inner)
	if sym == nil {
		return 0
	}
	count := 0
	for _, decl := range sym.Declarations {
		sf := ast.GetSourceFileOfNode(decl)
		if sf != nil && !sf.IsDeclarationFile {
			count++
		}
	}
	return count
}

// HeritageTypes returns the resolved types of every heritage entry
// (extends and implements clauses) on a class or interface declaration.
// Returns nil for non-class/interface nodes.
func (n *Node) HeritageTypes(c *Checker) []*Type {
	if n == nil || n.inner == nil || c == nil || c.inner == nil {
		return nil
	}
	var clauses *ast.NodeList
	switch n.inner.Kind {
	case ast.KindClassDeclaration, ast.KindClassExpression:
		clauses = n.inner.ClassLikeData().HeritageClauses
	case ast.KindInterfaceDeclaration:
		clauses = n.inner.AsInterfaceDeclaration().HeritageClauses
	default:
		return nil
	}
	if clauses == nil {
		return nil
	}
	var out []*Type
	for _, clause := range clauses.Nodes {
		if clause.Kind != ast.KindHeritageClause {
			continue
		}
		h := clause.AsHeritageClause()
		if h.Types == nil {
			continue
		}
		for _, typeNode := range h.Types.Nodes {
			t := c.inner.GetTypeFromTypeNode(typeNode)
			if t == nil {
				continue
			}
			out = append(out, &Type{inner: t, checker: c.inner})
		}
	}
	return out
}

// PropertyType returns the type of the named property on t, or nil
// if the property doesn't exist on the type.
func (t *Type) PropertyType(name string) *Type {
	if t == nil || t.inner == nil {
		return nil
	}
	for _, p := range t.checker.GetApparentProperties(t.inner) {
		if p.Name != name {
			continue
		}
		pt := t.checker.GetTypeOfSymbol(p)
		if pt == nil {
			return nil
		}
		return &Type{inner: pt, checker: t.checker}
	}
	return nil
}

// PropertySymbol returns the symbol of the named property on t, or
// nil if the type doesn't carry that property.
func (t *Type) PropertySymbol(name string) *Symbol {
	if t == nil || t.inner == nil {
		return nil
	}
	sym := t.checker.GetPropertyOfType(t.inner, name)
	if sym == nil {
		return nil
	}
	return &Symbol{inner: sym, checker: t.checker}
}

// HasNonPlainStringIndexSignature reports whether t has an index
// signature whose key type is more specific than plain `string` —
// e.g. a template-literal type like `\`key_${string}\`` or a
// transformer type like `Lowercase<string>`. Such signatures express
// a pattern; bracket access by a concrete string key is the only
// runtime form that can match them.
func (t *Type) HasNonPlainStringIndexSignature() bool {
	if t == nil || t.inner == nil {
		return false
	}
	for _, info := range t.checker.GetIndexInfosOfType(t.inner) {
		kt := info.KeyType()
		if kt == nil {
			continue
		}
		flags := kt.Flags()
		if flags&checker.TypeFlagsStringLike == 0 {
			continue
		}
		// Plain `string` matches `TypeFlagsString` exactly. Anything
		// narrower — string-literal, template-literal, intrinsic
		// string-mapped types — fails this exact-match check.
		if flags&checker.TypeFlagsString == 0 || flags&^checker.TypeFlagsString != 0 {
			return true
		}
	}
	return false
}

// HasIndexSignature reports whether t has an index signature whose
// key flags match the requested kind (`string`, `number`, or empty
// for any). Used by rules that allow bracket-notation access on
// indexable types.
func (t *Type) HasIndexSignature(kind string) bool {
	if t == nil || t.inner == nil {
		return false
	}
	for _, info := range t.checker.GetIndexInfosOfType(t.inner) {
		kt := info.KeyType()
		if kt == nil {
			continue
		}
		if kind == "" {
			return true
		}
		flags := kt.Flags()
		switch kind {
		case "string":
			if flags&checker.TypeFlagsStringLike != 0 {
				return true
			}
		case "number":
			if flags&checker.TypeFlagsNumberLike != 0 {
				return true
			}
		}
	}
	return false
}

// PropertyNames returns the names of every apparent property on the
// type. Intended for diagnostic introspection by rules that need to
// detect shape-based conventions (e.g. presence of Symbol.toPrimitive).
func (t *Type) PropertyNames() []string {
	if t == nil || t.inner == nil {
		return nil
	}
	props := t.checker.GetApparentProperties(t.inner)
	out := make([]string, 0, len(props))
	for _, p := range props {
		out = append(out, p.Name)
	}
	return out
}

// IdentifierResolvesToCatchBinding reports whether the identifier
// resolves to a variable bound by a catch clause (`catch (e)`) or
// to a parameter of a Promise `.catch(handler)` / `.then(_, handler)`
// callback. Used by rules that allow re-throwing caught values
// (only-throw-error, no-throw-literal).
func (n *Node) IdentifierResolvesToCatchBinding(c *Checker) bool {
	if n == nil || n.inner == nil || c == nil || c.inner == nil {
		return false
	}
	if n.inner.Kind != ast.KindIdentifier {
		return false
	}
	sym := c.inner.GetSymbolAtLocation(n.inner)
	if sym == nil {
		return false
	}
	for _, decl := range sym.Declarations {
		if decl.Kind == ast.KindVariableDeclaration && decl.Parent != nil &&
			decl.Parent.Kind == ast.KindCatchClause {
			return true
		}
		if decl.Kind == ast.KindParameter && isPromiseCatchCallbackParameter(decl) {
			return true
		}
	}
	return false
}

// isPromiseCatchCallbackParameter reports whether the parameter
// declaration belongs to an arrow/function expression that is being
// passed to a Promise `.catch(handler)` or `.then(_, handler)` call.
// Rejects rest parameters and calls with leading spread arguments —
// in those shapes, the parameter doesn't reliably hold the rejection.
func isPromiseCatchCallbackParameter(param *ast.Node) bool {
	pd := param.AsParameterDeclaration()
	if pd == nil {
		return false
	}
	if pd.DotDotDotToken != nil {
		// Rest parameter: e is an array, not the rejection value.
		return false
	}
	fn := param.Parent
	if fn == nil {
		return false
	}
	if fn.Kind != ast.KindArrowFunction && fn.Kind != ast.KindFunctionExpression {
		return false
	}
	call := fn.Parent
	if call == nil || call.Kind != ast.KindCallExpression {
		return false
	}
	callee := call.AsCallExpression().Expression
	if callee == nil || callee.Kind != ast.KindPropertyAccessExpression {
		return false
	}
	name := callee.AsPropertyAccessExpression().Name()
	if name == nil {
		return false
	}
	method := name.Text()
	args := call.AsCallExpression().Arguments
	if args == nil {
		return false
	}
	// Any spread argument before our function makes positional matching
	// unreliable.
	for _, a := range args.Nodes {
		if a == fn {
			break
		}
		if a.Kind == ast.KindSpreadElement {
			return false
		}
	}
	if method == "catch" && len(args.Nodes) >= 1 && args.Nodes[0] == fn {
		return true
	}
	if method == "then" && len(args.Nodes) >= 2 && args.Nodes[1] == fn {
		return true
	}
	return false
}

// FileHasTopLevelDeclaration reports whether the source file
// containing this node declares a top-level function, variable, class,
// or import binding with the given name. Cheap pre-check for
// shadow-detection in rules where tsgo's own symbol resolution may
// merge user functions with global ambients.
func (n *Node) FileHasTopLevelDeclaration(name string) bool {
	if n == nil || n.inner == nil || name == "" {
		return false
	}
	sf := ast.GetSourceFileOfNode(n.inner)
	if sf == nil {
		return false
	}
	for _, stmt := range sf.Statements.Nodes {
		switch stmt.Kind {
		case ast.KindFunctionDeclaration:
			if id := stmt.AsFunctionDeclaration().Name(); id != nil && id.Text() == name {
				return true
			}
		case ast.KindClassDeclaration:
			if id := stmt.AsClassDeclaration().Name(); id != nil && id.Text() == name {
				return true
			}
		case ast.KindVariableStatement:
			vs := stmt.AsVariableStatement()
			if vs.DeclarationList == nil {
				continue
			}
			for _, decl := range vs.DeclarationList.AsVariableDeclarationList().Declarations.Nodes {
				if decl.Kind != ast.KindVariableDeclaration {
					continue
				}
				if id := decl.AsVariableDeclaration().Name(); id != nil &&
					id.Kind == ast.KindIdentifier && id.Text() == name {
					return true
				}
			}
		case ast.KindImportDeclaration:
			ic := stmt.AsImportDeclaration().ImportClause
			if ic == nil {
				continue
			}
			cl := ic.AsImportClause()
			if cl.Name() != nil && cl.Name().Text() == name {
				return true
			}
			if cl.NamedBindings != nil {
				switch cl.NamedBindings.Kind {
				case ast.KindNamespaceImport:
					if id := cl.NamedBindings.AsNamespaceImport().Name(); id != nil && id.Text() == name {
						return true
					}
				case ast.KindNamedImports:
					for _, spec := range cl.NamedBindings.AsNamedImports().Elements.Nodes {
						if spec.Kind != ast.KindImportSpecifier {
							continue
						}
						if id := spec.AsImportSpecifier().Name(); id != nil && id.Text() == name {
							return true
						}
					}
				}
			}
		}
	}
	return false
}

// TaggedTemplateInterpolations returns the interpolated expression
// nodes (the `${expr}` parts) of a TaggedTemplateExpression in source
// order. Empty for non-tagged-template nodes or untemplated tags.
func (n *Node) TaggedTemplateInterpolations() []*Node {
	if n == nil || n.inner == nil || n.inner.Kind != ast.KindTaggedTemplateExpression {
		return nil
	}
	tmpl := n.inner.AsTaggedTemplateExpression().Template
	if tmpl == nil {
		return nil
	}
	if tmpl.Kind != ast.KindTemplateExpression {
		return nil
	}
	te := tmpl.AsTemplateExpression()
	if te.TemplateSpans == nil {
		return nil
	}
	out := make([]*Node, 0, len(te.TemplateSpans.Nodes))
	for _, span := range te.TemplateSpans.Nodes {
		if span.Kind != ast.KindTemplateSpan {
			continue
		}
		expr := span.AsTemplateSpan().Expression
		if expr != nil {
			out = append(out, &Node{inner: expr})
		}
	}
	return out
}

// IsImportedIdentifier reports whether the identifier resolves to a
// symbol declared by an import statement (ImportClause, ImportSpecifier,
// NamespaceImport).
func (n *Node) IsImportedIdentifier(c *Checker) bool {
	if n == nil || n.inner == nil || c == nil || c.inner == nil {
		return false
	}
	if n.inner.Kind != ast.KindIdentifier {
		return false
	}
	sym := c.inner.GetSymbolAtLocation(n.inner)
	if sym == nil {
		return false
	}
	for _, decl := range sym.Declarations {
		switch decl.Kind {
		case ast.KindImportSpecifier, ast.KindImportClause,
			ast.KindNamespaceImport, ast.KindImportEqualsDeclaration:
			return true
		}
	}
	return false
}

// SymbolIsAmbient reports whether the type's declaring symbol comes
// from a declaration file (lib.*.d.ts or installed @types). When true,
// the type is the global ambient one rather than a user-redeclared
// shadow with the same name.
func (t *Type) SymbolIsAmbient() bool {
	if t == nil || t.inner == nil {
		return false
	}
	sym := t.inner.Symbol()
	if sym == nil {
		return false
	}
	for _, decl := range sym.Declarations {
		sf := ast.GetSourceFileOfNode(decl)
		if sf == nil {
			continue
		}
		if sf.IsDeclarationFile {
			return true
		}
	}
	return false
}

// SymbolIsUserDeclared reports whether the type's declaring symbol has
// any declaration in user (non-.d.ts) source.
func (t *Type) SymbolIsUserDeclared() bool {
	if t == nil || t.inner == nil {
		return false
	}
	sym := t.inner.Symbol()
	if sym == nil {
		return false
	}
	for _, decl := range sym.Declarations {
		sf := ast.GetSourceFileOfNode(decl)
		if sf == nil {
			continue
		}
		if !sf.IsDeclarationFile {
			return true
		}
	}
	return false
}

// IsNumberLike reports whether the type is number, a number literal,
// or any other number-shaped type (including enum members).
func (t *Type) IsNumberLike() bool {
	if t == nil || t.inner == nil {
		return false
	}
	return t.inner.Flags()&checker.TypeFlagsNumberLike != 0
}

// NumericLiteralValue returns the literal numeric value if t is a
// number-literal type. Returns (0, false) for non-number-literal types.
func (t *Type) NumericLiteralValue() (float64, bool) {
	if t == nil || t.inner == nil {
		return 0, false
	}
	if t.inner.Flags()&checker.TypeFlagsNumberLiteral == 0 {
		return 0, false
	}
	v := t.inner.AsLiteralType().Value()
	if n, ok := v.(jsnum.Number); ok {
		return float64(n), true
	}
	return 0, false
}

// StringLiteralValue returns the literal string value if t is a
// string-literal type. Returns ("", false) for non-string-literal types.
func (t *Type) StringLiteralValue() (string, bool) {
	if t == nil || t.inner == nil {
		return "", false
	}
	if t.inner.Flags()&checker.TypeFlagsStringLiteral == 0 {
		return "", false
	}
	v := t.inner.AsLiteralType().Value()
	if s, ok := v.(string); ok {
		return s, true
	}
	return "", false
}

// IsBigIntLike reports whether the type is bigint or a bigint literal.
func (t *Type) IsBigIntLike() bool {
	if t == nil || t.inner == nil {
		return false
	}
	return t.inner.Flags()&checker.TypeFlagsBigIntLike != 0
}

// IsTypeParameter reports whether the type is a generic type parameter
// (the `T` in `<T>` before any constraint resolution).
func (t *Type) IsTypeParameter() bool {
	if t == nil || t.inner == nil {
		return false
	}
	return t.inner.Flags()&checker.TypeFlagsTypeParameter != 0
}

// EnumName returns the enum type's name for an enum-literal type
// (the parent enum's name) or for an enum type itself. Empty for
// non-enum types or when the parent symbol is missing.
func (t *Type) EnumName() string {
	if t == nil || t.inner == nil {
		return ""
	}
	sym := t.inner.Symbol()
	if sym == nil {
		return ""
	}
	if t.inner.Flags()&checker.TypeFlagsEnum != 0 {
		return sym.Name
	}
	if t.inner.Flags()&checker.TypeFlagsEnumLiteral != 0 {
		// Enum literal: the symbol is the enum member; its parent is
		// the enum type.
		if sym.Parent != nil {
			return sym.Parent.Name
		}
		return sym.Name
	}
	return ""
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

// IsESSymbolLike reports whether the type is a `symbol` (the
// primitive introduced in ES6) — including unique symbol literals.
func (t *Type) IsESSymbolLike() bool {
	if t == nil || t.inner == nil {
		return false
	}
	return t.inner.Flags()&checker.TypeFlagsESSymbolLike != 0
}

// IsNull reports whether the type is exactly `null`.
func (t *Type) IsNull() bool {
	if t == nil || t.inner == nil {
		return false
	}
	return t.inner.Flags()&checker.TypeFlagsNull != 0
}

// IsUndefined reports whether the type is exactly `undefined`.
func (t *Type) IsUndefined() bool {
	if t == nil || t.inner == nil {
		return false
	}
	return t.inner.Flags()&checker.TypeFlagsUndefined != 0
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
		// Tighten the check: the first parameter of `then` must be
		// callable (it's the `onFulfilled` callback). A no-arg `then()`
		// or one whose first parameter has no call signature isn't a
		// real thenable — typescript-eslint matches this stricter view
		// of `getAwaitedType`.
		for _, sig := range t.checker.GetCallSignatures(propType) {
			params := sig.Parameters()
			if len(params) == 0 {
				continue
			}
			firstT := t.checker.GetTypeOfSymbol(params[0])
			if firstT == nil {
				continue
			}
			if len(t.checker.GetCallSignatures(firstT)) > 0 {
				return true
			}
			// Allow `then(onFulfilled?: ((value: T) => unknown) | undefined)`:
			// the parameter type itself is a union with a callable member.
			if firstT.Flags()&checker.TypeFlagsUnion != 0 {
				for _, m := range firstT.Types() {
					if len(t.checker.GetCallSignatures(m)) > 0 {
						return true
					}
				}
			}
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
	// panics. Restrict to declared classes/interfaces — the cases we
	// care about are `class MyPromise extends Promise<T>` and
	// `interface Alias<T> extends Promise<X>`.
	if sym == nil || sym.Flags&(ast.SymbolFlagsClass|ast.SymbolFlagsInterface) == 0 {
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
		var clauses *ast.NodeList
		switch {
		case ast.IsClassDeclaration(decl) || ast.IsClassExpression(decl):
			clauses = decl.ClassLikeData().HeritageClauses
		case ast.IsInterfaceDeclaration(decl):
			clauses = decl.AsInterfaceDeclaration().HeritageClauses
		default:
			continue
		}
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

// BaseTypes returns the declared base types of a class or interface
// (the `extends` heritage). For generic instantiations whose
// TypeReference lacks resolved bases, it falls back to reading the
// declaration's heritage clauses. Empty for non-class/interface types.
func (t *Type) BaseTypes() []*Type {
	if t == nil || t.inner == nil {
		return nil
	}
	sym := t.inner.Symbol()
	if sym == nil || sym.Flags&(ast.SymbolFlagsClass|ast.SymbolFlagsInterface) == 0 {
		return nil
	}
	bases := safeGetBaseTypes(t.checker, t.inner)
	if len(bases) == 0 {
		bases = baseTypesFromClassDeclarations(t.checker, sym)
	}
	out := make([]*Type, 0, len(bases))
	for _, b := range bases {
		out = append(out, &Type{inner: b, checker: t.checker})
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

// Symbol returns the symbol the type refers to, or nil. Useful for
// rules that need to inspect the declaring node — e.g. checking
// whether a type is a class instance via its declaration's kind.
func (t *Type) Symbol() *Symbol {
	if t == nil || t.inner == nil {
		return nil
	}
	sym := t.inner.Symbol()
	if sym == nil {
		return nil
	}
	return &Symbol{inner: sym, checker: t.checker}
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
				// Only follow `extends` (structural inheritance for
				// interfaces, true class extension for classes).
				// `implements` is a constraint check, not a runtime
				// inheritance — instances don't carry the implemented
				// interface's prototype chain.
				if h.Token != ast.KindExtendsKeyword {
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
	// Constraint resolves to the same type — it's not a generic, just
	// signal absence so callers don't loop.
	if c == t.inner {
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

// ConstructSignatures returns the construct (`new`) signatures of the
// type. Empty for types that aren't `new`-able.
func (t *Type) ConstructSignatures() []*Signature {
	if t == nil || t.inner == nil {
		return nil
	}
	sigs := t.checker.GetSignaturesOfType(t.inner, checker.SignatureKindConstruct)
	out := make([]*Signature, 0, len(sigs))
	for _, s := range sigs {
		out = append(out, &Signature{inner: s, checker: t.checker})
	}
	return out
}

// IsAssignableTo reports whether this type is assignable to `target`.
// Uses the underlying checker's structural-assignability rules.
func (t *Type) IsAssignableTo(target *Type) bool {
	if t == nil || target == nil || t.inner == nil || target.inner == nil {
		return false
	}
	return t.checker.IsTypeAssignableTo(t.inner, target.inner)
}

// Equal reports whether this and other refer to the identical type
// instance — pointer equality on the underlying checker type. This is
// stricter than mutual assignability and matches the discriminator
// typescript-eslint uses for class-vs-this comparisons.
func (t *Type) Equal(other *Type) bool {
	if t == nil || other == nil {
		return t == other
	}
	return t.inner == other.inner
}

// GlobalErrorType returns the global `Error` type from lib.es5.d.ts.
// Nil when not in scope (extremely unusual).
func (c *Checker) GlobalErrorType() *Type {
	if c == nil || c.inner == nil {
		return nil
	}
	t := c.inner.GetGlobalType("Error", 0)
	if t == nil {
		return nil
	}
	return &Type{inner: t, checker: c.inner}
}

// HasNumericIndex reports whether the type has a numeric index
// signature (`{ [k: number]: V }`). Catches array-likes such as
// HTMLCollection, NodeList, IArguments that aren't full ReadonlyArray
// subtypes.
func (t *Type) HasNumericIndex() bool {
	if t == nil || t.inner == nil {
		return false
	}
	return t.checker.HasNumericIndexSignature(t.inner)
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
	// GetTypeArguments asserts the type is a TypeReference. Other type
	// shapes (intrinsic types, type parameters, anonymous object types
	// without resolution) panic if asked for arguments. Filter so
	// callers can ask freely.
	if t.inner.Flags()&checker.TypeFlagsObject == 0 {
		return nil
	}
	if t.inner.AsTypeReference() == nil {
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
		// Symbol.toPrimitive lives under a name like
		// "<sentinel>@toPrimitive@N" — the prefix is a non-printable
		// runtime sentinel. Match the well-known-symbol substring.
		if containsSubstring(p.Name, "@toPrimitive@") {
			return true
		}
	}
	return false
}

func containsSubstring(s, sub string) bool {
	if len(sub) == 0 {
		return true
	}
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
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
// SignatureDeclarationFile returns the file path of the signature's
// declaration site, or empty string when the signature has no
// declaration (synthesized by the checker). Callers can use this to
// distinguish a global ambient signature (in lib.*.d.ts) from a
// user-supplied one.
func (s *Signature) DeclarationIsUserSource() bool {
	if s == nil || s.inner == nil {
		return false
	}
	declFn := s.inner.Declaration
	if declFn == nil {
		return false
	}
	declNode := declFn()
	if declNode == nil {
		return false
	}
	sf := ast.GetSourceFileOfNode(declNode)
	if sf == nil {
		return false
	}
	return !sf.IsDeclarationFile
}

type Signature struct {
	inner   *checker.Signature
	checker *checker.Checker
}

// ParameterTypes returns the parameter types of the signature in
// declaration order. Used by rules that need to check the shape of a
// callable beyond its return type (e.g. a `then` method's first
// parameter must itself be callable).
func (s *Signature) ParameterTypes() []*Type {
	if s == nil || s.inner == nil {
		return nil
	}
	params := s.inner.Parameters()
	out := make([]*Type, 0, len(params))
	for _, p := range params {
		t := s.checker.GetTypeOfSymbol(p)
		if t == nil {
			out = append(out, nil)
			continue
		}
		out = append(out, &Type{inner: t, checker: s.checker})
	}
	return out
}

// MinArgumentCount returns the number of required parameters of the
// signature — i.e. the count of parameters declared without `?` or
// an initializer, before any rest parameter. This is the lower bound
// upstream uses when checking arity compatibility between callable
// shapes.
func (s *Signature) MinArgumentCount() int {
	if s == nil || s.inner == nil {
		return 0
	}
	count := 0
	for _, p := range s.inner.Parameters() {
		if p == nil {
			continue
		}
		decls := p.Declarations
		if len(decls) == 0 {
			count++
			continue
		}
		decl := decls[0]
		if decl == nil || !ast.IsParameterDeclaration(decl) {
			count++
			continue
		}
		pd := decl.AsParameterDeclaration()
		if pd == nil {
			count++
			continue
		}
		if pd.DotDotDotToken != nil {
			break
		}
		if pd.QuestionToken != nil || pd.Initializer != nil {
			break
		}
		count++
	}
	return count
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

// Declarations returns the declarations of the symbol as wrapper Nodes.
func (s *Symbol) Declarations() []*Node {
	if s == nil || s.inner == nil {
		return nil
	}
	out := make([]*Node, 0, len(s.inner.Declarations))
	for _, d := range s.inner.Declarations {
		out = append(out, &Node{inner: d})
	}
	return out
}

// IsReadonly reports whether the symbol is declared `readonly` (a
// property with the modifier, an enum member, a `const` variable, a
// getter without a corresponding setter, or a union/intersection
// where every constituent is readonly).
func (s *Symbol) IsReadonly() bool {
	if s == nil || s.inner == nil || s.checker == nil {
		return false
	}
	checkFlags := s.inner.CheckFlags & ast.CheckFlagsReadonly
	if checkFlags != 0 {
		return true
	}
	flags := s.inner.Flags
	if flags&ast.SymbolFlagsProperty != 0 {
		for _, d := range s.inner.Declarations {
			if ast.HasSyntacticModifier(d, ast.ModifierFlagsReadonly) {
				return true
			}
		}
	}
	if flags&ast.SymbolFlagsAccessor != 0 && flags&ast.SymbolFlagsSetAccessor == 0 {
		return true
	}
	if flags&ast.SymbolFlagsEnumMember != 0 {
		return true
	}
	return false
}

// TypeArgumentNodes returns the type argument TypeNodes of a node that
// has them — CallExpression, NewExpression, TaggedTemplateExpression,
// TypeReference, ExpressionWithTypeArguments, ImportType, TypeQuery,
// JsxOpeningElement, JsxSelfClosingElement. Empty for any other node
// or when no type arguments are present.
func (n *Node) TypeArgumentNodes() []*Node {
	if n == nil || n.inner == nil {
		return nil
	}
	switch n.inner.Kind {
	case ast.KindCallExpression,
		ast.KindNewExpression,
		ast.KindTaggedTemplateExpression,
		ast.KindTypeReference,
		ast.KindExpressionWithTypeArguments,
		ast.KindImportType,
		ast.KindTypeQuery,
		ast.KindJsxOpeningElement,
		ast.KindJsxSelfClosingElement:
	default:
		return nil
	}
	args := n.inner.TypeArguments()
	if len(args) == 0 {
		return nil
	}
	out := make([]*Node, 0, len(args))
	for _, a := range args {
		out = append(out, &Node{inner: a})
	}
	return out
}

// TypeReferenceTypeName returns the type name node of a TypeReference
// (the `Foo` in `Foo<X>`). Nil for other node kinds.
func (n *Node) TypeReferenceTypeName() *Node {
	if n == nil || n.inner == nil || n.inner.Kind != ast.KindTypeReference {
		return nil
	}
	name := n.inner.AsTypeReferenceNode().TypeName
	if name == nil {
		return nil
	}
	return &Node{inner: name}
}

// ExpressionWithTypeArgumentsExpression returns the inner expression of
// an ExpressionWithTypeArguments node (the `Foo` in `extends Foo<X>`).
// Nil for other node kinds.
func (n *Node) ExpressionWithTypeArgumentsExpression() *Node {
	if n == nil || n.inner == nil || n.inner.Kind != ast.KindExpressionWithTypeArguments {
		return nil
	}
	expr := n.inner.AsExpressionWithTypeArguments().Expression
	if expr == nil {
		return nil
	}
	return &Node{inner: expr}
}

// TypeParameterDeclarations returns the type parameter declaration
// nodes of a generic declaration (class/interface/typealias/function-
// like). Empty for non-generic or unsupported nodes.
func (n *Node) TypeParameterDeclarations() []*Node {
	if n == nil || n.inner == nil {
		return nil
	}
	var list *ast.NodeList
	switch n.inner.Kind {
	case ast.KindClassDeclaration:
		list = n.inner.AsClassDeclaration().TypeParameters
	case ast.KindClassExpression:
		list = n.inner.AsClassExpression().TypeParameters
	case ast.KindInterfaceDeclaration:
		list = n.inner.AsInterfaceDeclaration().TypeParameters
	case ast.KindTypeAliasDeclaration, ast.KindJSTypeAliasDeclaration:
		list = n.inner.AsTypeAliasDeclaration().TypeParameters
	default:
		if fn := n.inner.FunctionLikeData(); fn != nil {
			list = fn.TypeParameters
		}
	}
	if list == nil {
		return nil
	}
	out := make([]*Node, 0, len(list.Nodes))
	for _, tp := range list.Nodes {
		out = append(out, &Node{inner: tp})
	}
	return out
}

// TypeParameterDefaultType returns the default type AST node of a
// TypeParameterDeclaration (the `U` in `<T = U>`). Nil when the
// parameter has no default.
func (n *Node) TypeParameterDefaultType() *Node {
	if n == nil || n.inner == nil || n.inner.Kind != ast.KindTypeParameter {
		return nil
	}
	def := n.inner.AsTypeParameterDeclaration().DefaultType
	if def == nil {
		return nil
	}
	return &Node{inner: def}
}

// ResolvedSignatureGeneral returns the resolved signature for any
// invocation-like node (CallExpression, NewExpression,
// TaggedTemplateExpression, JsxOpeningElement, JsxSelfClosingElement).
// Returns nil for other nodes or when resolution fails.
func (c *Checker) ResolvedSignatureGeneral(n *Node) *Signature {
	if n == nil || n.inner == nil || c == nil || c.inner == nil {
		return nil
	}
	switch n.inner.Kind {
	case ast.KindCallExpression,
		ast.KindNewExpression,
		ast.KindTaggedTemplateExpression,
		ast.KindJsxOpeningElement,
		ast.KindJsxSelfClosingElement:
	default:
		return nil
	}
	sig := c.inner.GetResolvedSignature(n.inner)
	if sig == nil {
		return nil
	}
	return &Signature{inner: sig, checker: c.inner}
}

// SignatureDeclaration returns the AST declaration node of the
// signature (a function-like decl), or nil for synthesized signatures.
func (s *Signature) SignatureDeclaration() *Node {
	if s == nil || s.inner == nil {
		return nil
	}
	declFn := s.inner.Declaration
	if declFn == nil {
		return nil
	}
	d := declFn()
	if d == nil {
		return nil
	}
	return &Node{inner: d}
}

// SymbolDeclarations returns the declaration nodes of the type's
// declaring symbol, in source order. Empty when the type has no symbol
// (intrinsic types, anonymous structures).
func (t *Type) SymbolDeclarations() []*Node {
	if t == nil || t.inner == nil {
		return nil
	}
	sym := t.inner.Symbol()
	if sym == nil {
		return nil
	}
	out := make([]*Node, 0, len(sym.Declarations))
	for _, d := range sym.Declarations {
		if d == nil {
			continue
		}
		out = append(out, &Node{inner: d})
	}
	return out
}

// IsInDeclarationFile reports whether the node lives in a .d.ts
// declaration file (lib.*, ambient, types).
func (n *Node) IsInDeclarationFile() bool {
	if n == nil || n.inner == nil {
		return false
	}
	sf := ast.GetSourceFileOfNode(n.inner)
	return sf != nil && sf.IsDeclarationFile
}

// FunctionParameters returns the parameter declaration nodes of any
// function-like node (function/method/arrow/getter/setter/etc.). Empty
// for non-function nodes.
func (n *Node) FunctionParameters() []*Node {
	if n == nil || n.inner == nil {
		return nil
	}
	fn := n.inner.FunctionLikeData()
	if fn == nil || fn.Parameters == nil {
		return nil
	}
	out := make([]*Node, 0, len(fn.Parameters.Nodes))
	for _, p := range fn.Parameters.Nodes {
		out = append(out, &Node{inner: p})
	}
	return out
}

// ParameterName returns the binding-name node of a parameter
// declaration (typically an Identifier; can be a binding pattern).
// Nil for non-parameter nodes.
func (n *Node) ParameterName() *Node {
	if n == nil || n.inner == nil || n.inner.Kind != ast.KindParameter {
		return nil
	}
	pd := n.inner.AsParameterDeclaration()
	if pd == nil || pd.Name() == nil {
		return nil
	}
	return &Node{inner: pd.Name()}
}

// IsVoidTypeNode reports whether n is a TypeNode that names `void`.
func (n *Node) IsVoidTypeNode() bool {
	if n == nil || n.inner == nil {
		return false
	}
	return n.inner.Kind == ast.KindVoidKeyword
}

// SymbolValueDeclaration returns the symbol's primary value
// declaration — the declaration TypeScript treats as the canonical one
// for runtime semantics (e.g. the function expression assigned to a
// property field, the method body of a method declaration).
func (s *Symbol) SymbolValueDeclaration() *Node {
	if s == nil || s.inner == nil || s.inner.ValueDeclaration == nil {
		return nil
	}
	return &Node{inner: s.inner.ValueDeclaration}
}

// IsDeprecated reports whether any declaration of the symbol — or, if
// the symbol is an alias, any declaration along the alias chain — is
// marked with a `@deprecated` JSDoc tag. Useful for the no-deprecated
// rule, where a deprecated tag on the imported binding, the local
// import binding, or the original definition all count.
func (s *Symbol) IsDeprecated() bool {
	if s == nil || s.inner == nil {
		return false
	}
	if symbolHasDeprecatedDecl(s.inner) {
		return true
	}
	if s.checker == nil {
		return false
	}
	cur := s.inner
	for cur != nil && cur.Flags&ast.SymbolFlagsAlias != 0 {
		next := s.checker.GetImmediateAliasedSymbol(cur)
		if next == nil || next == cur {
			break
		}
		if symbolHasDeprecatedDecl(next) {
			return true
		}
		cur = next
	}
	return false
}

func symbolHasDeprecatedDecl(s *ast.Symbol) bool {
	if s == nil {
		return false
	}
	for _, d := range s.Declarations {
		if ast.IsDeprecatedDeclaration(d) {
			return true
		}
	}
	return false
}

// DeprecationReason returns the text of the first `@deprecated` tag's
// comment from any declaration of the symbol (walking the alias chain
// when the symbol is an alias). Empty when the symbol is not deprecated
// or when the tag carries no message.
func (s *Symbol) DeprecationReason() string {
	if s == nil || s.inner == nil {
		return ""
	}
	if r := deprecationReasonFromSymbol(s.inner); r != "" {
		return r
	}
	if s.checker == nil {
		return ""
	}
	cur := s.inner
	for cur != nil && cur.Flags&ast.SymbolFlagsAlias != 0 {
		next := s.checker.GetImmediateAliasedSymbol(cur)
		if next == nil || next == cur {
			break
		}
		if r := deprecationReasonFromSymbol(next); r != "" {
			return r
		}
		cur = next
	}
	return ""
}

func deprecationReasonFromSymbol(s *ast.Symbol) string {
	if s == nil {
		return ""
	}
	for _, d := range s.Declarations {
		if !ast.IsDeprecatedDeclaration(d) {
			continue
		}
		tag := ast.GetJSDocDeprecatedTag(d)
		if tag == nil {
			continue
		}
		return jsdocCommentText(tag.CommentList())
	}
	return ""
}

// IsDeprecated reports whether the signature's declaration is marked
// `@deprecated`. Useful when a symbol carries multiple overloads but
// only some are deprecated — the resolved signature pinpoints which.
func (s *Signature) IsDeprecated() bool {
	if s == nil || s.inner == nil {
		return false
	}
	declFn := s.inner.Declaration
	if declFn == nil {
		return false
	}
	d := declFn()
	if d == nil {
		return false
	}
	return ast.IsDeprecatedDeclaration(d)
}

// DeprecationReason returns the text of the signature's `@deprecated`
// tag, empty when the signature is not deprecated.
func (s *Signature) DeprecationReason() string {
	if s == nil || s.inner == nil {
		return ""
	}
	declFn := s.inner.Declaration
	if declFn == nil {
		return ""
	}
	d := declFn()
	if d == nil || !ast.IsDeprecatedDeclaration(d) {
		return ""
	}
	tag := ast.GetJSDocDeprecatedTag(d)
	if tag == nil {
		return ""
	}
	return jsdocCommentText(tag.CommentList())
}

// jsdocCommentText concatenates the text content of a JSDoc comment
// NodeList (which is a sequence of JSDocText/JSDocLink fragments).
func jsdocCommentText(list *ast.NodeList) string {
	if list == nil {
		return ""
	}
	var sb strings.Builder
	for _, n := range list.Nodes {
		if n == nil {
			continue
		}
		sb.WriteString(n.Text())
	}
	return sb.String()
}

// HeritageClauseToken returns the keyword Kind of a HeritageClause node
// (KindExtendsKeyword or KindImplementsKeyword). Zero for other nodes.
func (n *Node) HeritageClauseToken() Kind {
	if n == nil || n.inner == nil || n.inner.Kind != ast.KindHeritageClause {
		return 0
	}
	return Kind(n.inner.AsHeritageClause().Token)
}

// TypeOfSymbol returns the apparent type of a symbol, useful when the
// symbol refers to a value (e.g. `declare var Foo: { new <T>(...): any }`)
// and the caller needs the construct/call signatures of that type.
// NumberIndexType returns the type produced by indexing t with a
// number — the element type for arrays/tuples and the value type for
// objects with a numeric index signature. Walks type parameters and
// inherited base types so an interface extending `Array<T>` returns
// the substituted element type. Nil when t has no numeric index.
func (c *Checker) NumberIndexType(t *Type) *Type {
	if t == nil || t.inner == nil {
		return nil
	}
	idx := c.inner.GetNumericIndexType(t.inner)
	if idx == nil {
		return nil
	}
	return &Type{inner: idx, checker: c.inner}
}

// NumberIndexType is the Type-method form of Checker.NumberIndexType,
// using the type's own associated checker.
func (t *Type) NumberIndexType() *Type {
	if t == nil || t.checker == nil {
		return nil
	}
	idx := t.checker.GetNumericIndexType(t.inner)
	if idx == nil {
		return nil
	}
	return &Type{inner: idx, checker: t.checker}
}

func (c *Checker) TypeOfSymbol(s *Symbol) *Type {
	if c == nil || c.inner == nil || s == nil || s.inner == nil {
		return nil
	}
	t := c.inner.GetTypeOfSymbol(s.inner)
	if t == nil {
		return nil
	}
	return &Type{inner: t, checker: c.inner}
}

// Identical reports whether two wrapper Type values share the same
// underlying checker.Type pointer. Useful for comparing types by
// identity (the checker caches and de-duplicates types so identity
// comparison matches the upstream `===` checks used by typescript-eslint
// rules).
func (t *Type) Identical(other *Type) bool {
	if t == nil || other == nil || t.inner == nil || other.inner == nil {
		return false
	}
	return t.inner == other.inner
}

// --- internal helpers ---

type parseConfigHost struct {
	fs  vfs.FS
	cwd string
}

func (h *parseConfigHost) FS() vfs.FS                  { return h.fs }
func (h *parseConfigHost) GetCurrentDirectory() string { return h.cwd }

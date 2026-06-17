package main

// model-codegen: a headless type-query command for the effect-app codegen CLI.
//
// It builds a tsgo program from a tsconfig and, for each requested model, expands
// the model schema's `Encoded` member one level into a literal interface body --
// the native counterpart of the classic `typescript`-backed `type-resolver.ts`.
//
// Protocol: a single JSON request on stdin, a single JSON response on stdout.
//   request:  {"tsconfig": "...", "fileName": "...", "models": ["A","B"]}
//   response: {"ok": true, "blocks": {"A": "export namespace A { ... }"}}
//          |  {"ok": false, "error": "..."}
//
// This is the vertical-slice surface (Encoded only); the full printer port lives
// alongside the classic resolver's Type/Make/services/facade logic.

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
	"unsafe"

	"github.com/microsoft/typescript-go/shim/ast"
	"github.com/microsoft/typescript-go/shim/bundled"
	"github.com/microsoft/typescript-go/shim/checker"
	"github.com/microsoft/typescript-go/shim/compiler"
	"github.com/microsoft/typescript-go/shim/core"
	"github.com/microsoft/typescript-go/shim/tsoptions"
	"github.com/microsoft/typescript-go/shim/tspath"
	"github.com/microsoft/typescript-go/shim/vfs/cachedvfs"
	"github.com/microsoft/typescript-go/shim/vfs/osvfs"
)

// buildProgram parses the tsconfig and forces targetFile in as a program root
// (mirrors type-resolver.ts `roots = [...fileNames, ...files]`), so a model file
// a solution/base tsconfig does not directly include is still type-resolvable.
// Single-threaded => exactly one checker that owns every file.
func buildProgram(tsconfigPath string, host compiler.CompilerHost, targetFile string) (*compiler.Program, error) {
	cfg, _ := tsoptions.GetParsedCommandLineOfConfigFile(tsconfigPath, &core.CompilerOptions{}, nil, host, nil)
	if cfg == nil || cfg.ParsedConfig == nil {
		return nil, fmt.Errorf("could not parse tsconfig %s", tsconfigPath)
	}
	found := false
	for _, f := range cfg.ParsedConfig.FileNames {
		if f == targetFile {
			found = true
			break
		}
	}
	if !found {
		cfg.ParsedConfig.FileNames = append(cfg.ParsedConfig.FileNames, targetFile)
	}
	program := compiler.NewProgram(compiler.ProgramOptions{
		Config:                      cfg,
		SingleThreaded:              core.TSTrue,
		Host:                        host,
		UseSourceOfProjectReference: true,
	})
	if program == nil {
		return nil, fmt.Errorf("couldn't create program")
	}
	program.BindSourceFiles()
	return program, nil
}

type modelCodegenRequest struct {
	Tsconfig string             `json:"tsconfig"`
	FileName string             `json:"fileName"`
	Models   []string           `json:"models"`
	Options  modelCodegenOption `json:"options"`
}

// modelCodegenOption mirrors type-resolver.ts ResolveOptions.
type modelCodegenOption struct {
	Type   bool `json:"type"`
	Make   bool `json:"make"`
	Facade bool `json:"facade"`
}

type modelCodegenResponse struct {
	Ok     bool              `json:"ok"`
	Blocks map[string]string `json:"blocks,omitempty"`
	Error  string            `json:"error,omitempty"`
}

func writeModelCodegenResponse(resp modelCodegenResponse) int {
	out, err := json.Marshal(resp)
	if err != nil {
		fmt.Fprintf(os.Stdout, `{"ok":false,"error":"marshal failed: %v"}`, err)
		return 1
	}
	os.Stdout.Write(out)
	if resp.Ok {
		return 0
	}
	return 1
}

func runModelCodegen(args []string) int {
	cwd, err := os.Getwd()
	if err != nil {
		return writeModelCodegenResponse(modelCodegenResponse{Error: fmt.Sprintf("getwd: %v", err)})
	}

	raw, err := io.ReadAll(os.Stdin)
	if err != nil {
		return writeModelCodegenResponse(modelCodegenResponse{Error: fmt.Sprintf("read stdin: %v", err)})
	}
	var req modelCodegenRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return writeModelCodegenResponse(modelCodegenResponse{Error: fmt.Sprintf("parse request: %v", err)})
	}

	fs := bundled.WrapFS(cachedvfs.From(osvfs.FS()))
	resolvedConfig := tspath.ResolvePath(cwd, req.Tsconfig)
	configDir := tspath.GetDirectoryPath(resolvedConfig)
	host := compiler.NewCachedFSCompilerHost(configDir, fs, bundled.LibPath(), nil, nil)
	targetFile := tspath.ResolvePath(cwd, req.FileName)

	program, err := buildProgram(resolvedConfig, host, targetFile)
	if err != nil {
		return writeModelCodegenResponse(modelCodegenResponse{Error: err.Error()})
	}

	sf := program.GetSourceFile(targetFile)
	if sf == nil {
		return writeModelCodegenResponse(modelCodegenResponse{Error: fmt.Sprintf("file not in program: %s", req.FileName)})
	}

	// Single-threaded program => exactly one checker that owns every file.
	var ch *checker.Checker
	compiler.Program_ForEachCheckerParallel(program, func(idx int, c *checker.Checker) {
		if idx == 0 {
			ch = c
		}
	})
	if ch == nil {
		return writeModelCodegenResponse(modelCodegenResponse{Error: "no checker available"})
	}

	g := &modelGen{ch: ch, wanted: toSet(req.Models)}
	g.collectNames(sf)

	blocks := make(map[string]string, len(req.Models))
	for _, name := range req.Models {
		body, err := g.generate(name, req.Options)
		if err != nil {
			return writeModelCodegenResponse(modelCodegenResponse{Error: fmt.Sprintf("model %s: %v", name, err)})
		}
		blocks[name] = body
	}

	return writeModelCodegenResponse(modelCodegenResponse{Ok: true, Blocks: blocks})
}

type modelGen struct {
	ch           *checker.Checker
	wanted       map[string]struct{}
	schemaByName map[string]*ast.Node // model name -> name identifier node of its backing schema
	privateNames map[string]struct{}
}

// typeStr prints a type with NoTruncation, scoped to `atNode` (the source file
// node it will be emitted near), so tsgo picks a name VALID IN THAT FILE.
//
// `UseAliasDefinedOutsideCurrentScope` is the key flag: it lets tsgo resolve a
// symbol through the file's imports -- a directly-imported type stays bare
// (`NonNegativeNumber`), one reachable only via a namespace import becomes
// `S.StringId`, matching what the author wrote and what compiles. We do NOT use
// `UseFullyQualifiedType`, which in typescript-go emits absolute
// `import("/abs/path")` forms (machine-specific, non-portable).
func (g *modelGen) typeStr(t *checker.Type, atNode *ast.Node) string {
	return g.ch.TypeToStringEx(t, atNode, checker.TypeFormatFlagsNoTruncation|checker.TypeFormatFlagsUseAliasDefinedOutsideCurrentScope, nil)
}

func toSet(xs []string) map[string]struct{} {
	m := make(map[string]struct{}, len(xs))
	for _, x := range xs {
		m[x] = struct{}{}
	}
	return m
}

// collectNames mirrors type-resolver.ts `consider`: prefer the private `_X`
// (the real schema) over the self-referential exported facade `X`.
func (g *modelGen) collectNames(sf *ast.SourceFile) {
	g.schemaByName = make(map[string]*ast.Node)
	g.privateNames = make(map[string]struct{})

	consider := func(text string, nameNode *ast.Node) {
		if strings.HasPrefix(text, "_") && !strings.HasPrefix(text, "__") {
			base := text[1:]
			if _, ok := g.wanted[base]; ok {
				g.schemaByName[base] = nameNode
				g.privateNames[base] = struct{}{}
			}
			return
		}
		if _, ok := g.wanted[text]; ok {
			if _, isPriv := g.privateNames[text]; !isPriv {
				g.schemaByName[text] = nameNode
			}
		}
	}

	for _, n := range sf.Statements.Nodes {
		switch {
		case ast.IsClassDeclaration(n):
			if name := n.Name(); name != nil {
				consider(name.Text(), name)
			}
		case ast.IsVariableStatement(n):
			list := n.AsVariableStatement().DeclarationList.AsVariableDeclarationList()
			for _, d := range list.Declarations.Nodes {
				if name := d.Name(); name != nil && ast.IsIdentifier(name) {
					consider(name.Text(), name)
				}
			}
		}
	}
}

// generate mirrors type-resolver.ts `generate` for one model: the namespace
// (Encoded[/Type/Make/services]) or, in facade mode, the `export interface X`
// plus its namespace.
func (g *modelGen) generate(name string, opts modelCodegenOption) (string, error) {
	nameNode := g.schemaByName[name]
	if nameNode == nil {
		return "", fmt.Errorf("backing schema not found")
	}
	sym := g.ch.GetSymbolAtLocation(nameNode)
	if sym == nil {
		return "", fmt.Errorf("no symbol at backing schema")
	}
	schemaType := g.ch.GetTypeOfSymbolAtLocation(sym, nameNode)

	encoded, err := g.member(schemaType, "Encoded", nameNode)
	if err != nil {
		return "", err
	}

	emitType := opts.Facade || opts.Type || opts.Make
	emitMake := opts.Facade || opts.Make

	lines := make([]string, 0, 8)
	if !opts.Facade {
		lines = append(lines, fmt.Sprintf("export namespace %s {", name), fmt.Sprintf("  export interface Encoded %s", encoded))
	}
	if emitType {
		typ, err := g.member(schemaType, "Type", nameNode)
		if err != nil {
			return "", err
		}
		if opts.Facade {
			lines = append(lines, fmt.Sprintf("export interface %s %s", name, facadeType(typ)))
			lines = append(lines, fmt.Sprintf("export namespace %s {", name))
			lines = append(lines, fmt.Sprintf("  export interface Encoded %s", encoded))
		} else {
			lines = append(lines, fmt.Sprintf("  export interface Type %s", typ))
		}
	}
	if emitMake {
		mk, err := g.makeMember(schemaType, nameNode)
		if err != nil {
			return "", err
		}
		// A leading "=" marks a type-alias emission (e.g. `{...} | void`).
		if strings.HasPrefix(mk, "=") {
			lines = append(lines, fmt.Sprintf("  export type Make %s", mk))
		} else {
			lines = append(lines, fmt.Sprintf("  export interface Make %s", mk))
		}
	}
	if opts.Facade {
		dec, err := g.serviceMember(schemaType, "DecodingServices", nameNode)
		if err != nil {
			return "", err
		}
		enc, err := g.serviceMember(schemaType, "EncodingServices", nameNode)
		if err != nil {
			return "", err
		}
		lines = append(lines, fmt.Sprintf("  export type DecodingServices = %s", dec))
		lines = append(lines, fmt.Sprintf("  export type EncodingServices = %s", enc))
	}
	lines = append(lines, "}")
	return strings.Join(lines, "\n"), nil
}

// facadeType mirrors type-resolver.ts `facadeType`: strip `.Type` refs and
// outdent the Type-interface body by one level for the top-level `interface X`.
func facadeType(body string) string {
	body = strings.ReplaceAll(body, ".Type", "")
	body = strings.ReplaceAll(body, "\n    ", "\n  ")
	if strings.HasSuffix(body, "\n  }") {
		body = body[:len(body)-len("\n  }")] + "\n}"
	}
	return body
}

// member expands the top-level `Encoded` interface of schemaType one level,
// nested models referenced by name (`PrintMedia.Encoded`).
func (g *modelGen) member(schemaType *checker.Type, key string, atNode *ast.Node) (string, error) {
	memberSym := g.ch.GetPropertyOfType(schemaType, key)
	if memberSym == nil {
		return "", fmt.Errorf("no %s property on schema type", key)
	}
	memberType := g.ch.GetTypeOfSymbolAtLocation(memberSym, atNode)
	props := g.ch.GetPropertiesOfType(memberType)
	if len(props) == 0 {
		return fmt.Sprintf("extends %s {}", g.typeStr(memberType, atNode)), nil
	}
	fieldSyms := g.structFieldSymbols(atNode)
	lines := make([]string, 0, len(props))
	for _, p := range props {
		pt := g.ch.GetTypeOfSymbolAtLocation(p, atNode)
		opt := ""
		if p.Flags&ast.SymbolFlagsOptional != 0 {
			opt = "?"
		}
		// An expand-y field (union / non-facade struct) backed by a named schema
		// const -> reference it (`X.Encoded` / `typeof X.Encoded`) instead of
		// inline-expanding.
		val := g.fieldConstRef(fieldSyms, p.Name, pt, key)
		if val == "" {
			val = g.print(pt, atNode, key)
		}
		lines = append(lines, fmt.Sprintf("    readonly %s%s: %s", propKey(p.Name), opt, val))
	}
	return "{\n" + strings.Join(lines, "\n") + "\n  }", nil
}

// print mirrors type-resolver.ts `print`: walk composites by hand (so union
// member order follows declaration order, unlike TypeToString which sorts), and
// reference nested model namespaces by name instead of re-expanding them.
func (g *modelGen) print(t *checker.Type, atNode *ast.Node, side string) string {
	if side == "Encoded" {
		if mn := g.modelEncodedName(t); mn != "" {
			return mn
		}
	} else {
		if mn := g.modelTypeName(t); mn != "" {
			return mn
		}
	}
	// union -> walk members in declaration order
	if t.Flags()&checker.TypeFlagsUnion != 0 {
		parts := t.Types()
		out := make([]string, len(parts))
		for i, x := range parts {
			out[i] = g.print(x, atNode, side)
		}
		return strings.Join(out, " | ")
	}
	// tuple (e.g. NonEmptyArray -> readonly [E, ...E[]]) -> walk elements so nested
	// models become `Name.Encoded`/`Name.Type` (TypeToString prints them unqualified).
	if checker.IsTupleType(t) {
		args := checker.Checker_getTypeArguments(g.ch, t)
		flags := tupleElementFlags(t)
		parts := make([]string, len(args))
		for i, a := range args {
			el := g.print(a, atNode, side)
			var f checker.ElementFlags
			if i < len(flags) {
				f = flags[i]
			}
			switch {
			case f&checker.ElementFlagsRest != 0:
				parts[i] = "..." + asElement(el) + "[]"
			case f&checker.ElementFlagsOptional != 0:
				parts[i] = el + "?"
			default:
				parts[i] = el
			}
		}
		return "readonly [" + strings.Join(parts, ", ") + "]"
	}
	// array -> readonly E[]
	if checker.Checker_isArrayType(g.ch, t) {
		args := checker.Checker_getTypeArguments(g.ch, t)
		if len(args) == 1 {
			return "readonly " + asElement(g.print(args[0], atNode, side)) + "[]"
		}
	}
	// anonymous inline object -> expand structurally one level
	if g.isAnonymousObject(t) {
		props := g.ch.GetPropertiesOfType(t)
		if len(props) > 0 {
			parts := make([]string, len(props))
			for i, p := range props {
				pt := g.ch.GetTypeOfSymbolAtLocation(p, atNode)
				opt := ""
				if p.Flags&ast.SymbolFlagsOptional != 0 {
					opt = "?"
				}
				parts[i] = fmt.Sprintf("readonly %s%s: %s", propKey(p.Name), opt, g.print(pt, atNode, side))
			}
			// Multi-line: a deeply-nested inline object on one line can reach 50KB+
			// and crash the formatter; newline-separated members let it format. The
			// whitespace-insensitive codegen compare keeps this stable vs dprint.
			return "{\n" + strings.Join(parts, "\n") + "\n}"
		}
	}
	// primitives, literals, branded scalars, named library types
	printed := g.typeStr(t, atNode)
	if side == "Type" {
		return namedScalar(printed)
	}
	return printed
}

// typescript-go keeps per-tuple-element flags (rest/optional/fixed) in an
// unexported field. We read them via an exact struct mirror + unsafe.Pointer --
// the same technique tsgolint's own shim uses (TupleType_combinedFlags). Layout
// is fixed by the pinned typescript-go commit. Returns one flag per element.
type extraTupleType struct {
	checker.InterfaceType
	elementInfos  []checker.TupleElementInfo
	minLength     int
	fixedLength   int
	combinedFlags checker.ElementFlags
	readonly      bool
}

type extraTupleElementInfo struct {
	flags              checker.ElementFlags
	labeledDeclaration *ast.Node
}

func tupleElementFlags(t *checker.Type) []checker.ElementFlags {
	tt := t.TargetTupleType()
	if tt == nil {
		return nil
	}
	infos := (*extraTupleType)(unsafe.Pointer(tt)).elementInfos
	out := make([]checker.ElementFlags, len(infos))
	for i := range infos {
		out[i] = (*extraTupleElementInfo)(unsafe.Pointer(&infos[i])).flags
	}
	return out
}

// modelTypeName: if t is a model's instance type, return "Name.Type"; else "".
// Two shapes: Self = the class (symbol name == ModelName), or Self = `X.Type`
// (symbol name == "Type", parent == ModelName).
func (g *modelGen) modelTypeName(t *checker.Type) string {
	for _, sym := range []*ast.Symbol{aliasSymbolOf(t), t.Symbol()} {
		if sym == nil {
			continue
		}
		if sym.Name == "Type" && sym.Parent != nil && g.isModelParent(sym.Parent) {
			return sym.Parent.Name + ".Type"
		}
		if _, ok := g.wanted[sym.Name]; ok {
			return sym.Name + ".Type"
		}
	}
	return ""
}

// structFieldSymbols maps each struct field that is a DIRECT named-schema
// reference (`state: PickItemStateView`) to that schema's symbol, by walking the
// `_X` declaration's source AST. This is robust to `.pipe(encodeKeys/…)`, which
// strips the type-level `.fields`. Fields whose value is not a bare identifier
// (scalars, `NullOr(X)`, `NonEmptyArray(X)`, …) are omitted -> they expand.
func (g *modelGen) structFieldSymbols(nameNode *ast.Node) map[string]*ast.Symbol {
	out := map[string]*ast.Symbol{}
	nsym := g.ch.GetSymbolAtLocation(nameNode)
	if nsym == nil || nsym.ValueDeclaration == nil {
		return out
	}
	init := nsym.ValueDeclaration.Initializer()
	if init == nil {
		return out
	}
	var walk func(n *ast.Node)
	walk = func(n *ast.Node) {
		n.ForEachChild(func(c *ast.Node) bool {
			if ast.IsPropertyAssignment(c) {
				if nm, val := c.Name(), c.AsPropertyAssignment().Initializer; nm != nil && val != nil && ast.IsIdentifier(val) {
					fn := nm.Text()
					if _, exists := out[fn]; !exists {
						if s := g.ch.GetSymbolAtLocation(val); s != nil {
							out[fn] = checker.SkipAlias(s, g.ch)
						}
					}
				}
			}
			walk(c)
			return false
		})
	}
	walk(init)
	return out
}

// fieldConstRef: when a struct field's value would inline-expand (a union, or a
// non-facade model's structural object) AND it's backed by a named schema const,
// reference that const instead of expanding. Fallback chain (per `key`):
//  1. `X.Encoded` / `X` -- when the declaration exists (`namespace X { type
//     Encoded }` for Encoded, `type X` for Type),
//  2. else `typeof X.Encoded` / `typeof X.Type` -- the const is a schema, so the
//     query is valid and short,
//  3. else "" -- no const name; caller expands (multi-line).
func (g *modelGen) fieldConstRef(fieldSyms map[string]*ast.Symbol, prop string, computed *checker.Type, key string) string {
	if computed.Flags()&checker.TypeFlagsUnion == 0 && !g.isAnonymousObject(computed) {
		return ""
	}
	sym := fieldSyms[prop]
	if sym == nil || sym.Name == "" || sym.Name[0] < 'A' || sym.Name[0] > 'Z' {
		return ""
	}
	switch key {
	case "Encoded":
		if sym.Exports != nil {
			if _, ok := sym.Exports["Encoded"]; ok {
				return sym.Name + ".Encoded"
			}
		}
		return "typeof " + sym.Name + ".Encoded"
	case "Type":
		if sym.Flags&ast.SymbolFlagsTypeAlias != 0 {
			return sym.Name
		}
		return "typeof " + sym.Name + ".Type"
	case "Make":
		if sym.Exports != nil {
			if _, ok := sym.Exports["Make"]; ok {
				return sym.Name + ".Make"
			}
		}
		// No namespace `Make` -> the schema's make-input, by indexed access.
		return "typeof " + sym.Name + `["~type.make.in"]`
	}
	return ""
}

// namedScalar: `<base> & <Qualified>Brand` -> `<Qualified>` (the schema's
// companion scalar type alias).
var brandRe = regexp.MustCompile(`^[\w.\[\]"'| ]+ & ([\w.$]+)Brand$`)

func namedScalar(s string) string {
	if m := brandRe.FindStringSubmatch(s); m != nil {
		return m[1]
	}
	return s
}

// makeMember mirrors type-resolver.ts `makeMember`: keys/optionality from the
// make-input member (`~type.make.in`), values from the Type side with nested
// `.Type` rewritten to `.Make`. Preserves the `void` that effect-app keys off
// for no-arg `make()` (emitted as a type alias, signalled by a leading "=").
func (g *modelGen) makeMember(schemaType *checker.Type, atNode *ast.Node) (string, error) {
	makeSym := g.ch.GetPropertyOfType(schemaType, "~type.make.in")
	typeSym := g.ch.GetPropertyOfType(schemaType, "Type")
	if makeSym == nil || typeSym == nil {
		return "", fmt.Errorf("missing ~type.make.in or Type")
	}
	rawMakeType := g.ch.GetTypeOfSymbolAtLocation(makeSym, atNode)
	typeType := g.ch.GetTypeOfSymbolAtLocation(typeSym, atNode)

	isVoidish := func(t *checker.Type) bool {
		return t.Flags()&(checker.TypeFlagsVoid|checker.TypeFlagsUndefined) != 0
	}
	hasVoid := false
	makeType := rawMakeType
	if rawMakeType.Flags()&checker.TypeFlagsUnion != 0 {
		for _, x := range rawMakeType.Types() {
			if isVoidish(x) {
				hasVoid = true
			}
		}
		// Prefer the constituent with own properties (the `{...}` side).
		for _, x := range rawMakeType.Types() {
			if len(g.ch.GetPropertiesOfType(x)) > 0 {
				makeType = x
				break
			}
		}
	}

	makeProps := g.ch.GetPropertiesOfType(makeType)
	if len(makeProps) == 0 {
		return "= " + g.typeStr(rawMakeType, atNode), nil
	}

	typeByName := make(map[string]*ast.Symbol, len(makeProps))
	for _, p := range g.ch.GetPropertiesOfType(typeType) {
		typeByName[p.Name] = p
	}
	fieldSyms := g.structFieldSymbols(atNode)

	lines := make([]string, 0, len(makeProps))
	for _, p := range makeProps {
		opt := ""
		if p.Flags&ast.SymbolFlagsOptional != 0 {
			opt = "?"
		}
		source := p
		if s, ok := typeByName[p.Name]; ok {
			source = s
		}
		st := g.ch.GetTypeOfSymbolAtLocation(source, atNode)
		value := g.fieldConstRef(fieldSyms, p.Name, st, "Make")
		if value == "" {
			value = strings.ReplaceAll(g.print(st, atNode, "Type"), ".Type", ".Make")
		}
		lines = append(lines, fmt.Sprintf("    readonly %s%s: %s", propKey(p.Name), opt, value))
	}
	body := "{\n" + strings.Join(lines, "\n") + "\n  }"
	if hasVoid {
		return "= " + body + " | void", nil
	}
	return body, nil
}

func (g *modelGen) serviceMember(schemaType *checker.Type, key string, atNode *ast.Node) (string, error) {
	memberSym := g.ch.GetPropertyOfType(schemaType, key)
	if memberSym == nil {
		return "", fmt.Errorf("no %s on schema type", key)
	}
	return g.typeStr(g.ch.GetTypeOfSymbolAtLocation(memberSym, atNode), atNode), nil
}

// modelEncodedName: if t is a model's `Encoded` namespace interface, return
// "Name.Encoded"; else "".
// aliasSymbolOf returns the type's named-alias symbol (the `X` in `type X = …`),
// if any. A union model's `X.Encoded`/`X.Type` are type aliases, so their name
// lives here rather than on `t.Symbol()`.
func aliasSymbolOf(t *checker.Type) *ast.Symbol {
	if a := checker.Type_alias(t); a != nil {
		return a.Symbol()
	}
	return nil
}

// isModelParent: a symbol that names a model the file can reference (`X.Encoded`
// resolves) — one of this file's models, or any imported class/interface/namespace.
func (g *modelGen) isModelParent(p *ast.Symbol) bool {
	if _, ok := g.wanted[p.Name]; ok {
		return true
	}
	const modelLike = ast.SymbolFlagsClass | ast.SymbolFlagsInterface | ast.SymbolFlagsValueModule | ast.SymbolFlagsNamespaceModule
	return p.Flags&modelLike != 0
}

func (g *modelGen) modelEncodedName(t *checker.Type) string {
	// Check the alias symbol first (union models expose `X.Encoded` as a type
	// alias), then the plain symbol (struct models expose it as an interface).
	for _, sym := range []*ast.Symbol{aliasSymbolOf(t), t.Symbol()} {
		if sym == nil || sym.Name != "Encoded" || sym.Parent == nil {
			continue
		}
		if g.isModelParent(sym.Parent) {
			return sym.Parent.Name + ".Encoded"
		}
	}
	return ""
}

// isAnonymousObject: an inline object literal type (expand), vs a named
// interface/class (Date, branded scalars, library types) which we keep by name.
func (g *modelGen) isAnonymousObject(t *checker.Type) bool {
	if t.Flags()&checker.TypeFlagsObject == 0 {
		return false
	}
	if t.ObjectFlags()&checker.ObjectFlagsAnonymous == 0 {
		return false
	}
	sym := t.Symbol()
	return sym == nil || sym.Flags&(ast.SymbolFlagsTypeLiteral|ast.SymbolFlagsObjectLiteral) != 0
}

// asElement parenthesizes union/intersection (or readonly-prefixed) elements
// used as array elements, for precedence.
func asElement(s string) string {
	if strings.ContainsAny(s, "|&") || strings.HasPrefix(s, "readonly ") {
		return "(" + s + ")"
	}
	return s
}

func propKey(name string) string {
	for i, r := range name {
		ok := r == '_' || r == '$' || (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z')
		if i > 0 {
			ok = ok || (r >= '0' && r <= '9')
		}
		if !ok {
			b, _ := json.Marshal(name)
			return string(b)
		}
	}
	if name == "" {
		return `""`
	}
	return name
}

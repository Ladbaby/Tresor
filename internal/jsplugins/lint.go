package jsplugins

import (
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"

	"github.com/dop251/goja/ast"
	"github.com/dop251/goja/parser"
)

type LintDiagnostic struct {
	File     string `json:"file"`
	Line     int    `json:"line"`
	Column   int    `json:"column"`
	Code     string `json:"code"`
	Severity string `json:"severity"`
	Message  string `json:"message"`
}

// walkAST visits the pinned Goja AST, never source text or executed functions.
func walkAST(value any, visit func(ast.Node) bool) {
	var walk func(reflect.Value)
	walk = func(v reflect.Value) {
		if !v.IsValid() {
			return
		}
		if v.Kind() == reflect.Interface {
			if !v.IsNil() {
				walk(v.Elem())
			}
			return
		}
		if v.Kind() == reflect.Pointer {
			if v.IsNil() {
				return
			}
			if v.CanInterface() {
				if n, ok := v.Interface().(ast.Node); ok && !visit(n) {
					return
				}
			}
			v = v.Elem()
		}
		switch v.Kind() {
		case reflect.Struct:
			if v.Type().PkgPath() != "github.com/dop251/goja/ast" {
				return
			}
			for i := 0; i < v.NumField(); i++ {
				walk(v.Field(i))
			}
		case reflect.Slice:
			for i := 0; i < v.Len(); i++ {
				walk(v.Index(i))
			}
		}
	}
	walk(reflect.ValueOf(value))
}
func propertyName(expr ast.Expression) string {
	switch v := expr.(type) {
	case *ast.Identifier:
		return v.Name.String()
	case *ast.StringLiteral:
		return v.Value.String()
	}
	return ""
}
func dotted(expr ast.Expression) string {
	switch v := expr.(type) {
	case *ast.Identifier:
		return v.Name.String()
	case *ast.DotExpression:
		return dotted(v.Left) + "." + v.Identifier.Name.String()
	case *ast.BracketExpression:
		if s, ok := v.Member.(*ast.StringLiteral); ok {
			return dotted(v.Left) + "." + s.Value.String()
		}
	}
	return ""
}

// Lint is advisory static author guidance, not compatibility or permission proof.
func Lint(files map[string]string) ([]LintDiagnostic, error) {
	if _, err := SourceDigest(files); err != nil {
		return nil, err
	}
	var manifest struct {
		Entry string `json:"entry"`
	}
	// Author analysis needs only the entry path, not execution compatibility.
	// Full manifest validation remains the install-time contract.
	if err := json.Unmarshal([]byte(files["manifest.json"]), &manifest); err != nil {
		return nil, fmt.Errorf("manifest: %w", err)
	}
	out := []LintDiagnostic{}
	for name, source := range files {
		if !strings.HasSuffix(name, ".js") {
			continue
		}
		program, err := parser.ParseFile(nil, name, source, 0)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		emit := func(n ast.Node, code, message string) {
			p := program.File.Position(int(n.Idx0()) - 1)
			out = append(out, LintDiagnostic{name, p.Line, p.Column, code, "warning", message})
		}
		bindings := map[string]ast.Expression{}
		walkAST(program, func(n ast.Node) bool {
			switch v := n.(type) {
			case *ast.Binding:
				if id, ok := v.Target.(*ast.Identifier); ok {
					bindings[id.Name.String()] = v.Initializer
				}
			}
			return true
		})
		found := false
		var inspect func(ast.Expression, int)
		inspect = func(expr ast.Expression, depth int) {
			if depth > 8 {
				return
			}
			if id, ok := expr.(*ast.Identifier); ok {
				if e := bindings[id.Name.String()]; e != nil {
					inspect(e, depth+1)
					return
				}
			}
			obj, ok := expr.(*ast.ObjectLiteral)
			if !ok {
				emit(expr, "unresolved_exports", "Dynamic/imported exports cannot be fully checked without execution")
				return
			}
			for _, prop := range obj.Value {
				keyed, ok := prop.(*ast.PropertyKeyed)
				if !ok {
					emit(prop, "unresolved_exports", "Spread/shorthand exports cannot be fully resolved")
					continue
				}
				key := propertyName(keyed.Key)
				if keyed.Computed || !hooks[key] {
					emit(prop, "unresolved_hook", "Computed/unknown hook export is not statically checked")
					continue
				}
				e := keyed.Value
				for i := 0; i < 8; i++ {
					id, ok := e.(*ast.Identifier)
					if !ok {
						break
					}
					next := bindings[id.Name.String()]
					if next == nil {
						break
					}
					e = next
				}
				var params *ast.ParameterList
				var body ast.Node
				switch f := e.(type) {
				case *ast.FunctionLiteral:
					params = f.ParameterList
					body = f.Body
				case *ast.ArrowFunctionLiteral:
					params = f.ParameterList
					body = f.Body
				default:
					emit(prop, "unresolved_hook", "Hook function cannot be statically resolved")
					continue
				}
				if params.Rest != nil || len(params.List) > 1 {
					emit(params, "hook_parameters", "Use one named object parameter, not positional/rest arguments")
				}
				if len(params.List) == 0 {
					continue
				}
				aliases := map[string]bool{}
				switch p := params.List[0].Target.(type) {
				case *ast.ObjectPattern:
					if p.Rest != nil {
						emit(p, "host_input_rest", "Explicitly select consumed host fields; avoid host-input rest")
					}
				case *ast.Identifier:
					aliases[p.Name.String()] = true
					emit(p, "explicit_inputs", "Destructure consumed host fields explicitly")
				default:
					emit(p, "hook_parameters", "Use an object input parameter")
				}
				// Resolve obvious whole-input aliases conservatively. Provider field values are not aliases.
				for pass := 0; pass < 4; pass++ {
					walkAST(body, func(n ast.Node) bool {
						switch b := n.(type) {
						case *ast.Binding:
							if id, ok := b.Target.(*ast.Identifier); ok && aliases[dotted(b.Initializer)] {
								aliases[id.Name.String()] = true
							}
						case *ast.AssignExpression:
							if id, ok := b.Left.(*ast.Identifier); ok && aliases[dotted(b.Right)] {
								aliases[id.Name.String()] = true
							}
						}
						return true
					})
				}
				walkAST(body, func(n ast.Node) bool {
					switch v := n.(type) {
					case *ast.SpreadElement:
						if aliases[dotted(v.Expression)] {
							emit(v, "host_input_spread", "Do not spread the complete host input")
						}
					case *ast.ForInStatement:
						if aliases[dotted(v.Source)] {
							emit(v, "host_input_enumeration", "Do not enumerate complete host input")
						}
					case *ast.BracketExpression:
						if aliases[dotted(v.Left)] {
							if _, ok := v.Member.(*ast.StringLiteral); !ok {
								emit(v, "host_input_dynamic", "Use explicit host field names, not dynamic indexing")
							}
						}
					case *ast.CallExpression:
						callee := dotted(v.Callee)
						if callee == "Object.keys" || callee == "Object.values" || callee == "Object.entries" || callee == "Reflect.ownKeys" || callee == "Object.assign" {
							for _, arg := range v.ArgumentList {
								if aliases[dotted(arg)] {
									emit(v, "host_input_enumeration", "Do not enumerate/copy complete host input")
									break
								}
							}
						}
						for _, arg := range v.ArgumentList {
							if aliases[dotted(arg)] && callee != "Object.keys" && callee != "Object.entries" && callee != "Object.values" && callee != "Reflect.ownKeys" && callee != "Object.assign" {
								emit(v, "host_input_escape", "Whole input passed to helper; consumed fields are not statically resolved")
								break
							}
						}
					case *ast.Binding:
						if aliases[dotted(v.Initializer)] {
							if p, ok := v.Target.(*ast.ObjectPattern); ok && p.Rest != nil {
								emit(p, "host_input_rest", "Avoid host-input rest through aliases")
							}
						}
					}
					return true
				})
			}
		}
		walkAST(program, func(n ast.Node) bool {
			if a, ok := n.(*ast.AssignExpression); ok {
				path := dotted(a.Left)
				if path == "module.exports" {
					found = true
					inspect(a.Right, 0)
				} else if strings.HasPrefix(path, "exports.") || strings.HasPrefix(path, "module.exports.") {
					found = true
					emit(a, "unresolved_exports", "Incremental exports require manual author review")
				}
			}
			return true
		})
		if name == manifest.Entry && !found {
			emit(program, "unresolved_exports", "No statically resolvable CommonJS export assignment")
		}
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.File != b.File {
			return a.File < b.File
		}
		if a.Line != b.Line {
			return a.Line < b.Line
		}
		if a.Column != b.Column {
			return a.Column < b.Column
		}
		return a.Code < b.Code
	})
	return out, nil
}

// Copyright 2017 Felix Lange <fjl@twurst.com>.
// Use of this source code is governed by the MIT license,
// which can be found in the LICENSE file.

/*
Command gencodec generates marshaling methods for struct types.

When gencodec is invoked on a directory and type name, it creates a Go source file
containing JSON and YAML marshaling methods for the type. The generated methods add
features which the standard json package cannot offer.

	gencodec -dir . -type MyType -out mytype_json.go

Struct Tags

All fields are required unless the "optional" struct tag is present. The generated
unmarshaling method return an error if a required field is missing. Other struct tags are
carried over as is. The standard "json" and "yaml" tags can be used to rename a field when
marshaling to/from JSON.

Example:

	type foo {
		Required string
		Optional string `optional:""`
		Renamed  string `json:"otherName"`
	}

Field Type Overrides

An invocation of gencodec can specify an additional 'field override' struct from which
marshaling type replacements are taken. If the override struct contains a field whose name
matches the original type, the generated marshaling methods will use the overridden type
and convert to and from the original field type.

In this example, the specialString type implements json.Unmarshaler to enforce additional
parsing rules. When json.Unmarshal is used with type foo, the specialString unmarshaler
will be used to parse the value of SpecialField.

	//go:generate gencodec -dir . -type foo -field-override fooMarshaling -out foo_json.go

	type foo struct {
		Field        string
		SpecialField string
	}

	type fooMarshaling struct {
		SpecialField specialString // overrides type of SpecialField when marshaling/unmarshaling
	}

*/
package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"go/ast"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"io/ioutil"
	"os"
	"reflect"
	"strconv"
	"strings"
	"text/template"

	"golang.org/x/tools/imports"
)

func main() {
	var (
		pkgdir    = flag.String("dir", ".", "input package directory")
		output    = flag.String("out", "-", "output file")
		typename  = flag.String("type", "", "type to generate")
		overrides = flag.String("field-override", "", "type to take field type replacements from")
	)
	flag.Parse()

	fs := token.NewFileSet()
	pkg := loadPackage(fs, *pkgdir)
	code := makeMarshalingCode(fs, pkg, *typename, *overrides)
	if *output == "-" {
		os.Stdout.Write(code)
	} else if err := ioutil.WriteFile(*output, code, 0644); err != nil {
		fatal(err)
	}
}

func loadPackage(fs *token.FileSet, dir string) *types.Package {
	// Load the package.
	pkgs, err := parser.ParseDir(fs, dir, nil, parser.AllErrors)
	if err != nil {
		fatal(err)
	}
	if len(pkgs) == 0 || len(pkgs) > 1 {
		fatal(err)
	}
	var files []*ast.File
	var name string
	for _, pkg := range pkgs {
		for _, file := range pkg.Files {
			files = append(files, file)
		}
		name = pkg.Name
		break
	}
	// Type-check the package.
	cfg := types.Config{
		IgnoreFuncBodies: true,
		FakeImportC:      true,
		Importer:         importer.Default(),
	}
	tpkg, err := cfg.Check(name, fs, files, nil)
	if err != nil {
		fatal(err)
	}
	return tpkg
}

func makeMarshalingCode(fs *token.FileSet, pkg *types.Package, typename, otypename string) (packageBody []byte) {
	typ, err := lookupStructType(pkg.Scope(), typename)
	if err != nil {
		fatal(fmt.Sprintf("can't find %s: %v", typename, err))
	}
	mtyp := newMarshalerType(fs, pkg, typ)
	if otypename != "" {
		otyp, err := lookupStructType(pkg.Scope(), otypename)
		if err != nil {
			fatal(fmt.Sprintf("can't find field replacement type %s: %v", otypename, err))
		}
		mtyp.loadOverrides(otypename, otyp.Underlying().(*types.Struct))
	}

	w := new(bytes.Buffer)
	fmt.Fprintln(w, "// generated by gencodec, do not edit.\n")
	fmt.Fprintln(w, "package ", pkg.Name())
	fmt.Fprintln(w, render(mtyp.computeImports(), `
import (
{{- range $name, $path := . }}
	{{ $name }} "{{ $path }}"
{{- end }}
)`))
	fmt.Fprintln(w)
	fmt.Fprintln(w, mtyp.JSONMarshalMethod())
	fmt.Fprintln(w)
	fmt.Fprintln(w, mtyp.JSONUnmarshalMethod())
	fmt.Fprintln(w)
	fmt.Fprintln(w, mtyp.YAMLMarshalMethod())
	fmt.Fprintln(w)
	fmt.Fprintln(w, mtyp.YAMLUnmarshalMethod())

	// Use goimports to format the source because it separates imports.
	opt := &imports.Options{Comments: true, FormatOnly: true, TabIndent: true, TabWidth: 8}
	body, err := imports.Process("", w.Bytes(), opt)
	if err != nil {
		fatal("can't gofmt generated code:", err, "\n"+w.String())
	}
	return body
}

// marshalerType represents the intermediate struct type used during marshaling.
// This is the input data to all the Go code templates.
type marshalerType struct {
	OrigName string
	Name     string
	Fields   []*marshalerField
	fs       *token.FileSet
	orig     *types.Named
}

// marshalerField represents a field of the intermediate marshaling type.
type marshalerField struct {
	parent *marshalerType
	field  *types.Var
	typ    types.Type
	tag    string
}

func newMarshalerType(fs *token.FileSet, pkg *types.Package, typ *types.Named) *marshalerType {
	name := typ.Obj().Name() + "JSON"
	styp := typ.Underlying().(*types.Struct)
	mtyp := &marshalerType{OrigName: typ.Obj().Name(), Name: name, fs: fs, orig: typ}
	for i := 0; i < styp.NumFields(); i++ {
		f := styp.Field(i)
		if !f.Exported() {
			continue
		}
		mf := &marshalerField{parent: mtyp, field: f, typ: ensurePointer(f.Type()), tag: styp.Tag(i)}
		if f.Anonymous() {
			fmt.Fprintln(os.Stderr, mf.errorf("Warning: ignoring embedded field"))
			continue
		}
		mtyp.Fields = append(mtyp.Fields, mf)
	}
	return mtyp
}

// loadOverrides sets field types of the intermediate marshaling type from
// matching fields of otyp.
func (mtyp *marshalerType) loadOverrides(otypename string, otyp *types.Struct) {
	for i := 0; i < otyp.NumFields(); i++ {
		of := otyp.Field(i)
		if of.Anonymous() || !of.Exported() {
			fatalf("%v: field override type cannot have embedded or unexported fields", mtyp.fs.Position(of.Pos()))
		}
		f := mtyp.fieldByName(of.Name())
		if f == nil {
			fatalf("%v: no matching field for %s in original type %s", mtyp.fs.Position(of.Pos()), of.Name(), mtyp.OrigName)
		}
		if !types.ConvertibleTo(of.Type(), f.field.Type()) {
			fatalf("%v: field override type %s is not convertible to %s", mtyp.fs.Position(of.Pos()), mtyp.typeString(of.Type()), mtyp.typeString(f.field.Type()))
		}
		f.typ = ensurePointer(of.Type())
	}
}

func (mtyp *marshalerType) fieldByName(name string) *marshalerField {
	for _, f := range mtyp.Fields {
		if f.field.Name() == name {
			return f
		}
	}
	return nil
}

// computeImports returns the import paths of all referenced types.
// computeImports must be called before generating any code because it
// renames packages to avoid name clashes.
func (mtyp *marshalerType) computeImports() map[string]string {
	seen := make(map[string]string)
	counter := 0
	add := func(name string, path string, pkg *types.Package) {
		if seen[name] != path {
			if pkg != nil {
				name = "_" + name
				pkg.SetName(name)
			}
			if seen[name] != "" {
				// Name clash, add counter.
				name += "_" + strconv.Itoa(counter)
				counter++
				pkg.SetName(name)
			}
			seen[name] = path
		}
	}
	addNamed := func(typ *types.Named) {
		if pkg := typ.Obj().Pkg(); pkg != mtyp.orig.Obj().Pkg() {
			add(pkg.Name(), pkg.Path(), pkg)
		}
	}

	// Add packages which always referenced by the generated code.
	add("json", "encoding/json", nil)
	add("errors", "errors", nil)
	for _, f := range mtyp.Fields {
		// Add field types of the intermediate struct.
		walkNamedTypes(f.typ, addNamed)
		// Add field types of the original struct. Note that this won't generate unused
		// imports because all fields are either referenced by a conversion or by fields
		// of the intermediate struct (if no conversion is needed).
		walkNamedTypes(f.field.Type(), addNamed)
	}
	return seen
}

// JSONMarshalMethod generates MarshalJSON.
func (mtyp *marshalerType) JSONMarshalMethod() string {
	return render(mtyp, `
// MarshalJSON implements json.Marshaler.
func (x *{{.OrigName}}) MarshalJSON() ([]byte, error) {
	{{.TypeDecl}}

	return json.Marshal(&{{.Name}}{
		{{- range .Fields}}
			{{.Name}}: {{.Convert "x"}},
		{{- end}}
	})
}`)
}

// YAMLMarsalMethod generates MarshalYAML.
func (mtyp *marshalerType) YAMLMarshalMethod() string {
	return render(mtyp, `
// MarshalYAML implements yaml.Marshaler
func (x *{{.OrigName}}) MarshalYAML() (interface{}, error) {
	{{.TypeDecl}}

	return &{{.Name}}{
		{{- range .Fields}}
			{{.Name}}: {{.Convert "x"}},
		{{- end}}
	}, nil
}`)
}

// JSONUnmarshalMethod generates UnmarshalJSON.
func (mtyp *marshalerType) JSONUnmarshalMethod() string {
	return render(mtyp, `
// UnmarshalJSON implements json.Unmarshaler.
func (x *{{.OrigName}}) UnmarshalJSON(input []byte) error {
	{{.TypeDecl}}

	var dec {{.Name}}
	if err := json.Unmarshal(input, &dec); err != nil {
		return err
	}
	var v {{.OrigName}}
	{{.UnmarshalConversions "json"}}
	*x = v
	return nil
}`)
}

// YAMLUnmarshalMethod generates UnmarshalYAML.
func (mtyp *marshalerType) YAMLUnmarshalMethod() string {
	return render(mtyp, `
// UnmarshalYAML implements yaml.Unmarshaler.
func (x *{{.OrigName}}) UnmarshalYAML(fn func (interface{}) error) error {
	{{.TypeDecl}}

	var dec {{.Name}}
	if err := fn(&dec); err != nil {
		return err
	}
	var v {{.OrigName}}
	{{.UnmarshalConversions "yaml"}}
	*x = v
	return nil
}`)
}

// TypeDecl genereates the declaration of the intermediate marshaling type.
func (mtyp *marshalerType) TypeDecl() string {
	return render(mtyp, `
	type {{.Name}} struct{
		{{- range .Fields}}
			{{.Name}} {{.Type}} {{.StructTag}}
		{{- end}}
	}`)
}

// UnmarshalConversion genereates field conversions and presence checks.
func (mtyp *marshalerType) UnmarshalConversions(formatTag string) (s string) {
	type fieldContext struct{ Typ, Name, EncName, Conv string }

	for _, mf := range mtyp.Fields {
		ctx := fieldContext{
			Typ:     strings.ToUpper(formatTag) + " " + mtyp.OrigName,
			Name:    mf.Name(),
			EncName: mf.encodedName(formatTag),
			Conv:    mf.ConvertBack("dec"),
		}
		if mf.isOptional(formatTag) {
			s += render(ctx, `
				if dec.{{.Name}} != nil {
					v.{{.Name}} = {{.Conv}}
				}`)
		} else {
			s += render(ctx, `
				if dec.{{.Name}} == nil {
					return errors.New("missing required field '{{.EncName}}' in {{.Typ}}")
				}
				v.{{.Name}} = {{.Conv}}`)
		}
		s += "\n"
	}
	return s
}

func (mf *marshalerField) Name() string {
	return mf.field.Name()
}

func (mf *marshalerField) Type() string {
	return mf.parent.typeString(mf.typ)
}

func (mf *marshalerField) OrigType() string {
	return mf.parent.typeString(mf.typ)
}

func (mf *marshalerField) StructTag() string {
	if mf.tag == "" {
		return ""
	}
	return "`" + mf.tag + "`"
}

func (mf *marshalerField) Convert(variable string) string {
	expr := fmt.Sprintf("%s.%s", variable, mf.field.Name())
	return mf.parent.conversionExpr(expr, mf.field.Type(), mf.typ)
}

func (mf *marshalerField) ConvertBack(variable string) string {
	expr := fmt.Sprintf("%s.%s", variable, mf.field.Name())
	return mf.parent.conversionExpr(expr, mf.typ, mf.field.Type())
}

func (mtyp *marshalerType) conversionExpr(valueExpr string, from, to types.Type) string {
	if isPointer(from) && !isPointer(to) {
		valueExpr = "*" + valueExpr
		from = from.(*types.Pointer).Elem()
	} else if !isPointer(from) && isPointer(to) {
		valueExpr = "&" + valueExpr
		from = types.NewPointer(from)
	}
	if types.AssignableTo(from, to) {
		return valueExpr
	}
	return fmt.Sprintf("(%s)(%s)", mtyp.typeString(to), valueExpr)
}

func (mf *marshalerField) errorf(format string, args ...interface{}) error {
	pos := mf.parent.fs.Position(mf.field.Pos()).String()
	return errors.New(pos + ": (" + mf.parent.OrigName + "." + mf.Name() + ") " + fmt.Sprintf(format, args...))
}

// isOptional returns whether the field is optional when decoding the given format.
func (mf *marshalerField) isOptional(format string) bool {
	rtag := reflect.StructTag(mf.tag)
	if rtag.Get("optional") == "true" || rtag.Get("optional") == "yes" {
		return true
	}
	// Fields with json:"-" must be treated as optional.
	return strings.HasPrefix(rtag.Get(format), "-")
}

// encodedName returns the alternative field name assigned by the format's struct tag.
func (mf *marshalerField) encodedName(format string) string {
	val := reflect.StructTag(mf.tag).Get(format)
	if comma := strings.Index(val, ","); comma != -1 {
		val = val[:comma]
	}
	if val == "" || val == "-" {
		return uncapitalize(mf.Name())
	}
	return val
}

func (mtyp *marshalerType) typeString(typ types.Type) string {
	return types.TypeString(typ, func(pkg *types.Package) string {
		if pkg == mtyp.orig.Obj().Pkg() {
			return ""
		}
		return pkg.Name()
	})
}

// walkNamedTypes runs the callback for all named types contained in the given type.
func walkNamedTypes(typ types.Type, callback func(*types.Named)) {
	switch typ := typ.(type) {
	case *types.Basic:
	case *types.Chan:
		walkNamedTypes(typ.Elem(), callback)
	case *types.Map:
		walkNamedTypes(typ.Key(), callback)
		walkNamedTypes(typ.Elem(), callback)
	case *types.Named:
		callback(typ)
	case *types.Pointer:
		walkNamedTypes(typ.Elem(), callback)
	case *types.Slice:
		walkNamedTypes(typ.Elem(), callback)
	case *types.Struct:
		for i := 0; i < typ.NumFields(); i++ {
			walkNamedTypes(typ.Field(i).Type(), callback)
		}
	default:
		panic(fmt.Errorf("can't walk %T", typ))
	}
}

func lookupStructType(scope *types.Scope, name string) (*types.Named, error) {
	typ, err := lookupType(scope, name)
	if err != nil {
		return nil, err
	}
	_, ok := typ.Underlying().(*types.Struct)
	if !ok {
		return nil, errors.New("not a struct type")
	}
	return typ, nil
}

func lookupType(scope *types.Scope, name string) (*types.Named, error) {
	obj := scope.Lookup(name)
	if obj == nil {
		return nil, errors.New("no such identifier")
	}
	typ, ok := obj.(*types.TypeName)
	if !ok {
		return nil, errors.New("not a type")
	}
	return typ.Type().(*types.Named), nil
}

func isPointer(typ types.Type) bool {
	_, ok := typ.(*types.Pointer)
	return ok
}

func ensurePointer(typ types.Type) types.Type {
	if isPointer(typ) {
		return typ
	}
	return types.NewPointer(typ)
}

func uncapitalize(s string) string {
	return strings.ToLower(s[:1]) + s[1:]
}

func render(data interface{}, text string) string {
	t := template.Must(template.New("").Parse(strings.TrimSpace(text)))
	out := new(bytes.Buffer)
	if err := t.Execute(out, data); err != nil {
		panic(err)
	}
	return out.String()
}

func fatal(args ...interface{}) {
	fmt.Fprintln(os.Stderr, args...)
	os.Exit(1)
}

func fatalf(format string, args ...interface{}) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}

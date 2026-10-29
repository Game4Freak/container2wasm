package main

import (
	"fmt"
	"sort"
	"strings"
)

// generator turns a Resolve into the Rust source of the bridge component.
type generator struct {
	r        *Resolve
	world    *World
	worldIdx int
	out      strings.Builder
	types    strings.Builder // conversion functions, appended after out
	warnings []string

	// per type id conversion function names are derived from the id.
	emitted map[int]bool
	// interfaces of the world keyed by interface id, with direction.
	imports []int
	exports []int
}

func (g *generator) warnf(format string, args ...interface{}) {
	g.warnings = append(g.warnings, fmt.Sprintf(format, args...))
}

func (g *generator) pf(format string, args ...interface{}) {
	fmt.Fprintf(&g.out, format, args...)
}

func (g *generator) tf(format string, args ...interface{}) {
	fmt.Fprintf(&g.types, format, args...)
}

// source returns the complete generated Rust source.
func (g *generator) source() string {
	return g.out.String() + "// ---- JSON conversions of the WIT types ----\n\n" + g.types.String()
}

// ---- resolve helpers ----------------------------------------------------

func (g *generator) typ(id int) *TypeDef { return &g.r.Types[id] }

// deref follows type aliases (named or anonymous `type` kinds).
func (g *generator) deref(t TypeRef) TypeRef {
	for t.IsID && g.typ(t.ID).Kind.Kind == "type" {
		t = g.typ(t.ID).Kind.Elem
	}
	return t
}

func (g *generator) ifaceFullName(id int) string {
	it := &g.r.Interfaces[id]
	name := ""
	if it.Name != nil {
		name = *it.Name
	}
	if it.Package == nil {
		return name
	}
	ns, pkg, version := pkgName(g.r.Packages[*it.Package].Name)
	full := ns + ":" + pkg + "/" + name
	if version != "" {
		full += "@" + version
	}
	return full
}

// ifaceModPath returns the Rust module path of an interface's bindings.
func (g *generator) ifaceModPath(id int, exported bool) string {
	it := &g.r.Interfaces[id]
	parts := []string{"crate", "generated"}
	if exported {
		parts = append(parts, "exports")
	}
	if it.Package != nil {
		ns, pkg, _ := pkgName(g.r.Packages[*it.Package].Name)
		parts = append(parts, snake(ns), snake(pkg))
	}
	if it.Name != nil {
		parts = append(parts, snake(*it.Name))
	}
	return strings.Join(parts, "::")
}

func (g *generator) isExportedIface(id int) bool {
	for _, e := range g.exports {
		if e == id {
			return true
		}
	}
	return false
}

// typePath returns the Rust path of a named type.
func (g *generator) typePath(id int) string {
	td := g.typ(id)
	name := upperCamel(*td.Name)
	if td.Owner != nil && td.Owner.Interface != nil {
		return g.ifaceModPath(*td.Owner.Interface, g.isExportedIface(*td.Owner.Interface)) + "::" + name
	}
	// world-level type
	return "crate::generated::" + name
}

// rustType returns the owned Rust type of a WIT type.
func (g *generator) rustType(t TypeRef) (string, error) {
	if !t.Valid {
		return "()", nil
	}
	if !t.IsID {
		switch t.Prim {
		case "bool":
			return "bool", nil
		case "u8", "u16", "u32", "u64":
			return t.Prim, nil
		case "s8":
			return "i8", nil
		case "s16":
			return "i16", nil
		case "s32":
			return "i32", nil
		case "s64":
			return "i64", nil
		case "f32", "float32":
			return "f32", nil
		case "f64", "float64":
			return "f64", nil
		case "char":
			return "char", nil
		case "string":
			return "String", nil
		}
		return "", fmt.Errorf("unsupported primitive type %q", t.Prim)
	}
	td := g.typ(t.ID)
	if td.Name != nil && td.Kind.Kind != "type" {
		return g.typePath(t.ID), nil
	}
	if td.Name != nil && td.Kind.Kind == "type" {
		// Named alias: wit-bindgen emits `pub type Name = ...`; use the
		// underlying type to keep conversions uniform.
		return g.rustType(td.Kind.Elem)
	}
	k := &td.Kind
	switch k.Kind {
	case "type":
		return g.rustType(k.Elem)
	case "list":
		e, err := g.rustType(k.Elem)
		if err != nil {
			return "", err
		}
		return "Vec<" + e + ">", nil
	case "option":
		e, err := g.rustType(k.Elem)
		if err != nil {
			return "", err
		}
		return "Option<" + e + ">", nil
	case "result":
		ok, err := g.rustType(k.Ok)
		if err != nil {
			return "", err
		}
		er, err := g.rustType(k.Err)
		if err != nil {
			return "", err
		}
		return "Result<" + ok + ", " + er + ">", nil
	case "tuple":
		var parts []string
		for _, e := range k.Tuple {
			s, err := g.rustType(e)
			if err != nil {
				return "", err
			}
			parts = append(parts, s)
		}
		return "(" + strings.Join(parts, ", ") + ",)", nil
	}
	return "", fmt.Errorf("unsupported type kind %q", k.Kind)
}

// needsOwnership reports whether a value of the type holds heap data
// (strings or lists), which decides how wit-bindgen borrows parameters of
// synchronous imports.
func (g *generator) needsOwnership(t TypeRef) bool {
	if !t.Valid {
		return false
	}
	if !t.IsID {
		return t.Prim == "string"
	}
	k := &g.typ(t.ID).Kind
	switch k.Kind {
	case "type":
		return g.needsOwnership(k.Elem)
	case "list":
		return true
	case "option":
		return g.needsOwnership(k.Elem)
	case "result":
		return g.needsOwnership(k.Ok) || g.needsOwnership(k.Err)
	case "tuple":
		for _, e := range k.Tuple {
			if g.needsOwnership(e) {
				return true
			}
		}
		return false
	case "record":
		for _, f := range k.Record {
			if g.needsOwnership(f.Type) {
				return true
			}
		}
		return false
	case "variant":
		for _, c := range k.Variant {
			if g.needsOwnership(c.Type) {
				return true
			}
		}
		return false
	}
	return false
}

// borrowExpr converts `e`, an expression of type `&T`, into the borrowed
// form wit-bindgen expects for parameters of synchronous imports.
func (g *generator) borrowExpr(e string, t TypeRef) string {
	if !t.Valid {
		return "()"
	}
	if !t.IsID {
		if t.Prim == "string" {
			return "(" + e + ").as_str()"
		}
		return "*(" + e + ")"
	}
	td := g.typ(t.ID)
	k := &td.Kind
	switch k.Kind {
	case "type":
		return g.borrowExpr(e, k.Elem)
	case "list":
		return "(" + e + ").as_slice()"
	case "option":
		return "(" + e + ").as_ref().map(|v| " + g.borrowExpr("v", k.Elem) + ")"
	case "result":
		return "(" + e + ").as_ref().map(|v| " + g.borrowExpr("v", k.Ok) + ").map_err(|v| " + g.borrowExpr("v", k.Err) + ")"
	case "tuple":
		var parts []string
		for i, el := range k.Tuple {
			parts = append(parts, g.borrowExpr(fmt.Sprintf("(&%s.%d)", e, i), el))
		}
		return "(" + strings.Join(parts, ", ") + ",)"
	case "record", "variant":
		if g.needsOwnership(t) {
			return e
		}
		return "*(" + e + ")"
	default: // enum, flags
		return "*(" + e + ")"
	}
}

// ---- JSON conversion functions ------------------------------------------

// toJSON returns an expression converting `e` (of type &T) to serde_json::Value.
func (g *generator) toJSON(e string, t TypeRef) string {
	if !t.Valid {
		return "Value::Null"
	}
	if !t.IsID {
		switch t.Prim {
		case "string":
			return "Value::String((" + e + ").clone())"
		case "f32", "float32", "f64", "float64":
			return "rt::float_to_json(*(" + e + ") as f64)"
		case "char":
			return "Value::String((" + e + ").to_string())"
		default:
			return "json!(*(" + e + "))"
		}
	}
	return fmt.Sprintf("t%d_to_json(%s)", t.ID, e)
}

// fromJSON returns an expression converting `e` (of type &Value) to
// Result<T, String>.
func (g *generator) fromJSON(e string, t TypeRef) string {
	if !t.Valid {
		return "Ok::<(), String>(())"
	}
	if !t.IsID {
		switch t.Prim {
		case "bool":
			return "rt::bool_from_json(" + e + ")"
		case "u8", "u16", "u32", "u64":
			return fmt.Sprintf("rt::uint_from_json::<%s>(%s, %s)", t.Prim, e, t.Prim[1:])
		case "s8", "s16", "s32", "s64":
			return fmt.Sprintf("rt::sint_from_json::<i%s>(%s, %s)", t.Prim[1:], e, t.Prim[1:])
		case "f32", "float32":
			return "rt::f32_from_json(" + e + ")"
		case "f64", "float64":
			return "rt::f64_from_json(" + e + ")"
		case "char":
			return "rt::char_from_json(" + e + ")"
		case "string":
			return "rt::string_from_json(" + e + ")"
		}
	}
	return fmt.Sprintf("t%d_from_json(%s)", t.ID, e)
}

func (g *generator) isU8(t TypeRef) bool {
	t = g.deref(t)
	return t.Valid && !t.IsID && t.Prim == "u8"
}

// emitTypeFns emits the conversion functions of a type and, recursively, of
// the types it references.
func (g *generator) emitTypeFns(t TypeRef) error {
	if !t.Valid || !t.IsID || g.emitted[t.ID] {
		return nil
	}
	g.emitted[t.ID] = true
	td := g.typ(t.ID)
	k := &td.Kind
	rty, err := g.rustType(t)
	if err != nil {
		return err
	}
	id := t.ID
	// Recurse first so helper functions exist (order does not matter in
	// Rust but keeps errors early).
	var children []TypeRef
	switch k.Kind {
	case "type", "list", "option":
		children = append(children, k.Elem)
	case "result":
		children = append(children, k.Ok, k.Err)
	case "tuple":
		children = append(children, k.Tuple...)
	case "record":
		for _, f := range k.Record {
			children = append(children, f.Type)
		}
	case "variant":
		for _, c := range k.Variant {
			children = append(children, c.Type)
		}
	case "enum", "flags":
	default:
		return fmt.Errorf("type kind %q is not supported by the bridge", k.Kind)
	}
	for _, c := range children {
		if err := g.emitTypeFns(c); err != nil {
			return err
		}
	}

	g.tf("fn t%d_to_json(v: &%s) -> Value {\n", id, rty)
	switch k.Kind {
	case "type":
		g.tf("    %s\n", g.toJSON("v", k.Elem))
	case "list":
		if g.isU8(k.Elem) {
			g.tf("    rt::bytes_to_json(v)\n")
		} else {
			g.tf("    Value::Array(v.iter().map(|x| %s).collect())\n", g.toJSON("x", k.Elem))
		}
	case "option":
		g.tf("    match v { None => Value::Null, Some(x) => %s }\n", g.toJSON("x", k.Elem))
	case "result":
		g.tf("    match v { Ok(x) => json!({\"ok\": %s}), Err(x) => json!({\"err\": %s}) }\n",
			g.toJSON("x", k.Ok), g.toJSON("x", k.Err))
	case "tuple":
		g.tf("    Value::Array(vec![")
		for i, el := range k.Tuple {
			g.tf("%s, ", g.toJSON(fmt.Sprintf("(&v.%d)", i), el))
		}
		g.tf("])\n")
	case "record":
		g.tf("    let mut m = Map::new();\n")
		for _, f := range k.Record {
			g.tf("    m.insert(%q.to_string(), %s);\n", f.Name, g.toJSON("(&v."+snake(f.Name)+")", f.Type))
		}
		g.tf("    Value::Object(m)\n")
	case "variant":
		g.tf("    match v {\n")
		for _, c := range k.Variant {
			if c.Type.Valid {
				g.tf("        %s::%s(x) => json!({\"tag\": %q, \"val\": %s}),\n", rty, upperCamel(c.Name), c.Name, g.toJSON("x", c.Type))
			} else {
				g.tf("        %s::%s => json!({\"tag\": %q}),\n", rty, upperCamel(c.Name), c.Name)
			}
		}
		g.tf("    }\n")
	case "enum":
		g.tf("    match v {\n")
		for _, c := range k.Enum {
			g.tf("        %s::%s => Value::String(%q.to_string()),\n", rty, upperCamel(c), c)
		}
		g.tf("    }\n")
	case "flags":
		g.tf("    let mut out = Vec::new();\n")
		for _, f := range k.Flags {
			g.tf("    if v.contains(%s::%s) { out.push(Value::String(%q.to_string())); }\n", rty, shouty(f), f)
		}
		g.tf("    Value::Array(out)\n")
	}
	g.tf("}\n\n")

	g.tf("fn t%d_from_json(v: &Value) -> Result<%s, String> {\n", id, rty)
	switch k.Kind {
	case "type":
		g.tf("    %s\n", g.fromJSON("v", k.Elem))
	case "list":
		if g.isU8(k.Elem) {
			g.tf("    rt::bytes_from_json(v)\n")
		} else {
			g.tf("    rt::array(v)?.iter().map(|x| %s).collect()\n", g.fromJSON("x", k.Elem))
		}
	case "option":
		inner := g.deref(k.Elem)
		if inner.IsID && g.typ(inner.ID).Kind.Kind == "option" {
			return fmt.Errorf("nested option types (option<option<T>>) are not supported by the bridge")
		}
		g.tf("    if v.is_null() { return Ok(None); }\n")
		g.tf("    Ok(Some(%s?))\n", g.fromJSON("v", k.Elem))
	case "result":
		g.tf("    let o = rt::object(v)?;\n")
		g.tf("    if let Some(x) = o.get(\"ok\") { return Ok(Ok(%s?)); }\n", g.fromJSON("x", k.Ok))
		g.tf("    if let Some(x) = o.get(\"err\") { return Ok(Err(%s?)); }\n", g.fromJSON("x", k.Err))
		g.tf("    Err(\"expected object with \\\"ok\\\" or \\\"err\\\"\".to_string())\n")
	case "tuple":
		g.tf("    let a = rt::array(v)?;\n")
		g.tf("    Ok((")
		for i, el := range k.Tuple {
			g.tf("%s?, ", g.fromJSON(fmt.Sprintf("rt::tuple_elem(a, %d, %d)?", i, len(k.Tuple)), el))
		}
		g.tf("))\n")
	case "record":
		g.tf("    let o = rt::object(v)?;\n")
		g.tf("    Ok(%s {\n", rty)
		for _, f := range k.Record {
			g.tf("        %s: %s?,\n", snake(f.Name), g.fromJSON(fmt.Sprintf("rt::field(o, %q)?", f.Name), f.Type))
		}
		g.tf("    })\n")
	case "variant":
		g.tf("    let o = rt::object(v)?;\n")
		g.tf("    let tag = rt::string_from_json(rt::field(o, \"tag\")?)?;\n")
		g.tf("    match tag.as_str() {\n")
		for _, c := range k.Variant {
			if c.Type.Valid {
				g.tf("        %q => Ok(%s::%s(%s?)),\n", c.Name, rty, upperCamel(c.Name), g.fromJSON("rt::field(o, \"val\")?", c.Type))
			} else {
				g.tf("        %q => Ok(%s::%s),\n", c.Name, rty, upperCamel(c.Name))
			}
		}
		g.tf("        other => Err(format!(\"unknown variant case {other:?}\")),\n")
		g.tf("    }\n")
	case "enum":
		g.tf("    match rt::string_from_json(v)?.as_str() {\n")
		for _, c := range k.Enum {
			g.tf("        %q => Ok(%s::%s),\n", c, rty, upperCamel(c))
		}
		g.tf("        other => Err(format!(\"unknown enum case {other:?}\")),\n")
		g.tf("    }\n")
	case "flags":
		g.tf("    let mut out = %s::empty();\n", rty)
		g.tf("    for x in rt::array(v)? {\n")
		g.tf("        match rt::string_from_json(x)?.as_str() {\n")
		for _, f := range k.Flags {
			g.tf("            %q => out |= %s::%s,\n", f, rty, shouty(f))
		}
		g.tf("            other => return Err(format!(\"unknown flag {other:?}\")),\n")
		g.tf("        }\n")
		g.tf("    }\n")
		g.tf("    Ok(out)\n")
	}
	g.tf("}\n\n")
	return nil
}

// ---- functions ------------------------------------------------------------

// supportedFunc checks that a function only uses supported types.
func (g *generator) supportedFunc(f *Function) error {
	if f.Kind.Other {
		return fmt.Errorf("resource methods and constructors are not supported")
	}
	check := func(t TypeRef) error {
		if !t.Valid {
			return nil
		}
		if _, err := g.rustType(t); err != nil {
			return err
		}
		return nil
	}
	for _, p := range f.Params {
		if err := check(p.Type); err != nil {
			return err
		}
	}
	return check(f.Result)
}

type ifaceFuncs struct {
	id    int    // interface id or -1 for world level functions
	name  string // JSON interface name
	path  string // Rust module path of the bindings
	funcs []Function
}

func (g *generator) collect(items map[string]WorldItem, exported bool) ([]ifaceFuncs, error) {
	var out []ifaceFuncs
	world := ifaceFuncs{id: -1, name: "", path: "crate::generated"}
	if exported {
		world.path = "crate::generated"
	}
	keys := make([]string, 0, len(items))
	for k := range items {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		it := items[k]
		switch {
		case it.Interface != nil:
			id := it.Interface.ID
			iface := &g.r.Interfaces[id]
			fnames := make([]string, 0, len(iface.Functions))
			for n := range iface.Functions {
				fnames = append(fnames, n)
			}
			sort.Strings(fnames)
			entry := ifaceFuncs{id: id, name: g.ifaceFullName(id), path: g.ifaceModPath(id, exported)}
			for _, n := range fnames {
				entry.funcs = append(entry.funcs, iface.Functions[n])
			}
			out = append(out, entry)
		case it.Function != nil:
			world.funcs = append(world.funcs, *it.Function)
		}
	}
	if len(world.funcs) > 0 {
		out = append(out, world)
	}
	return out, nil
}

func (g *generator) generate() error {
	g.emitted = map[int]bool{}
	for _, it := range g.world.Exports {
		if it.Interface != nil {
			g.exports = append(g.exports, it.Interface.ID)
		}
	}
	for _, it := range g.world.Imports {
		if it.Interface != nil {
			g.imports = append(g.imports, it.Interface.ID)
		}
	}
	imports, err := g.collect(g.world.Imports, false)
	if err != nil {
		return err
	}
	exports, err := g.collect(g.world.Exports, true)
	if err != nil {
		return err
	}

	g.pf("// Generated by wit-bridge-gen from world %q. DO NOT EDIT.\n\n", g.world.Name)
	g.pf("use crate::runtime as rt;\n")
	g.pf("use serde_json::{json, Map, Value};\n\n")
	g.pf("wit_bindgen::generate!({\n    world: \"bridge\",\n    path: \"wit\",\n    generate_all,\n});\n\n")
	g.pf("pub struct Component;\nexport!(Component);\n\n")
	g.pf("impl exports::wasi::cli::run::Guest for Component {\n")
	g.pf("    async fn run() -> Result<(), ()> {\n        rt::run().await\n    }\n}\n\n")

	// Guest -> host dispatch.
	g.pf("pub async fn dispatch(iface: &str, func: &str, args: &[Value]) -> Result<Value, String> {\n")
	g.pf("    match (iface, func) {\n")
	for _, entry := range imports {
		if entry.id >= 0 && strings.HasPrefix(entry.name, "c2w:bridge/") {
			continue
		}
		for i := range entry.funcs {
			f := &entry.funcs[i]
			if err := g.supportedFunc(f); err != nil {
				g.warnf("skipping import %s.%s: %v", entry.name, f.Name, err)
				continue
			}
			if err := g.emitFuncTypes(f); err != nil {
				return err
			}
			g.pf("        (%q, %q) => {\n", entry.name, f.Name)
			var callArgs []string
			for i, p := range f.Params {
				g.pf("            let p%d: %s = %s?;\n", i, must(g.rustType(p.Type)), g.fromJSON(fmt.Sprintf("rt::arg(args, %d, %q)?", i, p.Name), p.Type))
				if f.Kind.Async {
					callArgs = append(callArgs, fmt.Sprintf("p%d", i))
				} else {
					callArgs = append(callArgs, g.borrowExpr(fmt.Sprintf("&p%d", i), p.Type))
				}
			}
			await := ""
			if f.Kind.Async {
				await = ".await"
			}
			g.pf("            let r = %s::%s(%s)%s;\n", entry.path, snake(f.Name), strings.Join(callArgs, ", "), await)
			g.pf("            Ok(%s)\n", g.toJSON("&r", f.Result))
			g.pf("        }\n")
		}
	}
	g.pf("        _ => Err(format!(\"unknown function {iface}.{func}\")),\n")
	g.pf("    }\n}\n\n")

	// Host -> guest exports.
	for _, entry := range exports {
		if entry.id >= 0 && strings.HasPrefix(entry.name, "wasi:cli/run") {
			continue
		}
		trait := entry.path + "::Guest"
		if entry.id < 0 {
			trait = "crate::generated::Guest"
		}
		g.pf("impl %s for Component {\n", trait)
		for i := range entry.funcs {
			f := &entry.funcs[i]
			if err := g.supportedFunc(f); err != nil {
				return fmt.Errorf("export %s.%s: %v", entry.name, f.Name, err)
			}
			if !f.Kind.Async {
				return fmt.Errorf("export %s.%s: exported functions must be declared `async` in WIT because the guest answers asynchronously", entry.name, f.Name)
			}
			if err := g.emitFuncTypes(f); err != nil {
				return err
			}
			var params []string
			var jsonArgs []string
			for i, p := range f.Params {
				params = append(params, fmt.Sprintf("p%d: %s", i, must(g.rustType(p.Type))))
				jsonArgs = append(jsonArgs, g.toJSON(fmt.Sprintf("&p%d", i), p.Type))
			}
			ret := must(g.rustType(f.Result))
			g.pf("    async fn %s(%s) -> %s {\n", snake(f.Name), strings.Join(params, ", "), ret)
			g.pf("        let args = vec![%s];\n", strings.Join(jsonArgs, ", "))
			g.pf("        let r = rt::invoke(%q, %q, args).await;\n", entry.name, f.Name)
			g.pf("        let v = match r {\n")
			g.pf("            Ok(v) => v,\n")
			if g.isStringErrResult(f.Result) {
				g.pf("            Err(e) => return Err(e),\n")
			} else {
				g.pf("            Err(e) => rt::guest_error(%q, %q, &e),\n", entry.name, f.Name)
			}
			g.pf("        };\n")
			g.pf("        match %s {\n", g.fromJSON("&v", f.Result))
			g.pf("            Ok(x) => x,\n")
			g.pf("            Err(e) => rt::guest_error(%q, %q, &e),\n", entry.name, f.Name)
			g.pf("        }\n")
			g.pf("    }\n")
		}
		g.pf("}\n\n")
	}
	return nil
}

// isStringErrResult reports whether t is result<_, string>, in which case
// guest side errors are returned as the error value.
func (g *generator) isStringErrResult(t TypeRef) bool {
	t = g.deref(t)
	if !t.Valid || !t.IsID {
		return false
	}
	k := &g.typ(t.ID).Kind
	if k.Kind != "result" {
		return false
	}
	e := g.deref(k.Err)
	return e.Valid && !e.IsID && e.Prim == "string"
}

func (g *generator) emitFuncTypes(f *Function) error {
	for _, p := range f.Params {
		if err := g.emitTypeFns(p.Type); err != nil {
			return err
		}
	}
	return g.emitTypeFns(f.Result)
}

func must(s string, err error) string {
	if err != nil {
		panic(err)
	}
	return s
}

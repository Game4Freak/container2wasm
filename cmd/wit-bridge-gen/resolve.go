package main

import (
	"encoding/json"
	"fmt"
	"strings"
)

// The structures below mirror the JSON produced by
// `wasm-tools component wit --json` (the wit-parser Resolve).

type Resolve struct {
	Worlds     []World     `json:"worlds"`
	Interfaces []Interface `json:"interfaces"`
	Types      []TypeDef   `json:"types"`
	Packages   []Package   `json:"packages"`
}

type World struct {
	Name    string               `json:"name"`
	Imports map[string]WorldItem `json:"imports"`
	Exports map[string]WorldItem `json:"exports"`
	Package *int                 `json:"package"`
}

type WorldItem struct {
	Interface *struct {
		ID int `json:"id"`
	} `json:"interface"`
	Function *Function `json:"function"`
	Type     *int      `json:"type"`
}

type Interface struct {
	Name      *string             `json:"name"`
	Types     map[string]int      `json:"types"`
	Functions map[string]Function `json:"functions"`
	Package   *int                `json:"package"`
}

type Function struct {
	Name   string  `json:"name"`
	Kind   Kind    `json:"kind"`
	Params []Param `json:"params"`
	Result TypeRef `json:"result"`
}

// Kind is either a plain string ("freestanding", "async-freestanding") or
// an object for methods and constructors of resources.
type Kind struct {
	Name  string
	Async bool
	Other bool
}

func (k *Kind) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		k.Name = s
		k.Async = strings.HasPrefix(s, "async")
		return nil
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		return err
	}
	for key := range m {
		k.Name = key
		k.Async = strings.HasPrefix(key, "async")
	}
	k.Other = true
	return nil
}

type Param struct {
	Name string  `json:"name"`
	Type TypeRef `json:"type"`
}

// TypeRef is either a primitive type name, an index into Resolve.Types or
// absent (null).
type TypeRef struct {
	Prim  string
	ID    int
	IsID  bool
	Valid bool
}

func (t *TypeRef) UnmarshalJSON(b []byte) error {
	if string(b) == "null" {
		return nil
	}
	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		t.Prim = s
		t.Valid = true
		return nil
	}
	var n int
	if err := json.Unmarshal(b, &n); err != nil {
		return fmt.Errorf("unexpected type reference %s", string(b))
	}
	t.ID = n
	t.IsID = true
	t.Valid = true
	return nil
}

type TypeDef struct {
	Name  *string `json:"name"`
	Kind  TypeKind
	Owner *Owner `json:"owner"`
}

func (t *TypeDef) UnmarshalJSON(b []byte) error {
	var raw struct {
		Name  *string         `json:"name"`
		Kind  json.RawMessage `json:"kind"`
		Owner *Owner          `json:"owner"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	t.Name = raw.Name
	t.Owner = raw.Owner
	return t.Kind.UnmarshalJSON(raw.Kind)
}

type Owner struct {
	Interface *int `json:"interface"`
	World     *int `json:"world"`
}

// TypeKind holds exactly one of the fields depending on the WIT type.
type TypeKind struct {
	Kind    string // record, variant, enum, flags, tuple, list, option, result, type, handle, resource, stream, future, fixed-size-list, error-context, unknown
	Record  []Field
	Variant []Case
	Enum    []string
	Flags   []string
	Tuple   []TypeRef
	Elem    TypeRef // list, option, type (alias), stream, future
	Ok, Err TypeRef // result
}

type Field struct {
	Name string  `json:"name"`
	Type TypeRef `json:"type"`
}

type Case struct {
	Name string  `json:"name"`
	Type TypeRef `json:"type"`
}

func (k *TypeKind) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		k.Kind = s // e.g. "resource", "error-context"
		return nil
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		return err
	}
	for key, val := range m {
		k.Kind = key
		switch key {
		case "record":
			var r struct {
				Fields []Field `json:"fields"`
			}
			if err := json.Unmarshal(val, &r); err != nil {
				return err
			}
			k.Record = r.Fields
		case "variant":
			var v struct {
				Cases []Case `json:"cases"`
			}
			if err := json.Unmarshal(val, &v); err != nil {
				return err
			}
			k.Variant = v.Cases
		case "enum":
			var e struct {
				Cases []struct {
					Name string `json:"name"`
				} `json:"cases"`
			}
			if err := json.Unmarshal(val, &e); err != nil {
				return err
			}
			for _, c := range e.Cases {
				k.Enum = append(k.Enum, c.Name)
			}
		case "flags":
			var f struct {
				Flags []struct {
					Name string `json:"name"`
				} `json:"flags"`
			}
			if err := json.Unmarshal(val, &f); err != nil {
				return err
			}
			for _, c := range f.Flags {
				k.Flags = append(k.Flags, c.Name)
			}
		case "tuple":
			var t struct {
				Types []TypeRef `json:"types"`
			}
			if err := json.Unmarshal(val, &t); err != nil {
				return err
			}
			k.Tuple = t.Types
		case "list", "option", "type", "stream", "future":
			if err := json.Unmarshal(val, &k.Elem); err != nil {
				return err
			}
		case "result":
			var r struct {
				Ok  TypeRef `json:"ok"`
				Err TypeRef `json:"err"`
			}
			if err := json.Unmarshal(val, &r); err != nil {
				return err
			}
			k.Ok, k.Err = r.Ok, r.Err
		default:
			// handle, fixed-size-list, map, ...: unsupported, kept as kind name
		}
	}
	return nil
}

type Package struct {
	Name       string         `json:"name"`
	Interfaces map[string]int `json:"interfaces"`
	Worlds     map[string]int `json:"worlds"`
}

// pkgName splits "ns:pkg@1.2.3" into its parts.
func pkgName(full string) (ns, pkg, version string) {
	if i := strings.Index(full, "@"); i >= 0 {
		version = full[i+1:]
		full = full[:i]
	}
	parts := strings.SplitN(full, ":", 2)
	if len(parts) != 2 {
		return "", full, version
	}
	return parts[0], parts[1], version
}

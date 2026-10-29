package main

import (
	"strings"
	"unicode"
)

// Naming rules of wit-bindgen's Rust generator (crates/rust/src/lib.rs).

var rustKeywords = map[string]bool{
	"as": true, "break": true, "const": true, "continue": true, "crate": true, "else": true,
	"enum": true, "extern": true, "false": true, "fn": true, "for": true, "if": true, "impl": true,
	"in": true, "let": true, "loop": true, "match": true, "mod": true, "move": true, "mut": true,
	"pub": true, "ref": true, "return": true, "self": true, "static": true, "struct": true,
	"super": true, "trait": true, "true": true, "type": true, "unsafe": true, "use": true,
	"where": true, "while": true, "async": true, "await": true, "dyn": true, "abstract": true,
	"become": true, "box": true, "do": true, "final": true, "macro": true, "override": true,
	"priv": true, "typeof": true, "unsized": true, "virtual": true, "yield": true, "try": true,
}

// words splits a WIT identifier (kebab-case, possibly with %-escapes or
// digits) into lower-case words.
func words(name string) []string {
	name = strings.TrimPrefix(name, "%")
	var out []string
	for _, w := range strings.Split(name, "-") {
		if w != "" {
			out = append(out, strings.ToLower(w))
		}
	}
	return out
}

// snake converts a WIT identifier to snake_case and escapes Rust keywords
// like wit-bindgen's to_rust_ident.
func snake(name string) string {
	s := strings.Join(words(name), "_")
	if rustKeywords[s] {
		return s + "_"
	}
	return s
}

// upperCamel converts a WIT identifier to UpperCamelCase like heck does.
func upperCamel(name string) string {
	var b strings.Builder
	for _, w := range words(name) {
		r := []rune(w)
		r[0] = unicode.ToUpper(r[0])
		b.WriteString(string(r))
	}
	s := b.String()
	if s == "Guest" {
		return "Guest_"
	}
	return s
}

// shouty converts a WIT identifier to SHOUTY_SNAKE_CASE (bitflags consts).
func shouty(name string) string {
	return strings.ToUpper(strings.Join(words(name), "_"))
}

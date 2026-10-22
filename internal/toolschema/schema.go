// Package toolschema prepares and validates tool JSON without losing numbers
// or loading schema references from the network or filesystem.
package toolschema

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"sort"

	"ccdp/internal/jsonvalue"
	jsonschema "github.com/santhosh-tekuri/jsonschema/v6"
)

type Schema struct {
	compiled *jsonschema.Schema
}

type noExternalDocuments struct{}

func (noExternalDocuments) Load(url string) (any, error) {
	return nil, fmt.Errorf("external schema reference is not allowed: %s", url)
}

// Decode preserves exact JSON numbers and rejects trailing data.
func Decode(raw []byte) (any, error) {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	var value any
	if err := d.Decode(&value); err != nil {
		return nil, err
	}
	var extra any
	if err := d.Decode(&extra); err != io.EOF {
		if err == nil {
			err = fmt.Errorf("multiple JSON values")
		}
		return nil, err
	}
	return value, nil
}

// Compile owns an independent, immutable copy. Each tool registration owns
// its validators; replacing a tool cannot change an outstanding lease.
func Compile(document map[string]any) (*Schema, error) {
	return compile(document, true)
}

// CompileValidation preserves JSON Schema's annotation-only default keyword.
// Output schemas validate existing data and never prepare missing arguments.
func CompileValidation(document map[string]any) (*Schema, error) {
	return compile(document, false)
}

func compile(document map[string]any, prepare bool) (*Schema, error) {
	raw, err := json.Marshal(document)
	if err != nil {
		return nil, err
	}
	value, err := Decode(raw)
	if err != nil {
		return nil, err
	}
	compiler := jsonschema.NewCompiler()
	compiler.DefaultDraft(jsonschema.Draft2020)
	compiler.UseLoader(noExternalDocuments{})
	const location = "https://ccdp.invalid/tool-schema"
	if err := compiler.AddResource(location, value); err != nil {
		return nil, err
	}
	compiled, err := compiler.Compile(location)
	if err != nil {
		return nil, err
	}
	if prepare {
		if err := checkDefaults(compiled, map[defaultVisit]bool{}, false); err != nil {
			return nil, err
		}
	}
	return &Schema{compiled: compiled}, nil
}

// Defaults are filled through object properties, array items and ordinary $ref.
// Defaults inside conditional or combined schemas are ambiguous and rejected;
// validation still supports these constructs when they do not add defaults.
type defaultVisit struct {
	schema      *jsonschema.Schema
	conditional bool
}

func checkDefaults(s *jsonschema.Schema, seen map[defaultVisit]bool, conditional bool) error {
	key := defaultVisit{s, conditional}
	if s == nil || seen[key] {
		return nil
	}
	seen[key] = true
	if s.Default != nil {
		if conditional {
			return fmt.Errorf("default in conditional/combined schema is unsupported: %s", s.Location)
		}
		if err := s.Validate(*s.Default); err != nil {
			return fmt.Errorf("invalid schema default at %s: %w", s.Location, err)
		}
	}
	for _, child := range s.Properties {
		if err := checkDefaults(child, seen, conditional); err != nil {
			return err
		}
	}
	for _, child := range s.PatternProperties {
		if err := checkDefaults(child, seen, conditional); err != nil {
			return err
		}
	}
	if child, ok := s.AdditionalProperties.(*jsonschema.Schema); ok {
		if err := checkDefaults(child, seen, conditional); err != nil {
			return err
		}
	}
	for _, child := range []*jsonschema.Schema{s.Ref, s.Items2020} {
		if err := checkDefaults(child, seen, conditional); err != nil {
			return err
		}
	}
	switch items := s.Items.(type) {
	case *jsonschema.Schema:
		if err := checkDefaults(items, seen, conditional); err != nil {
			return err
		}
	case []*jsonschema.Schema:
		for _, item := range items {
			if err := checkDefaults(item, seen, conditional); err != nil {
				return err
			}
		}
	}
	for _, item := range s.PrefixItems {
		if err := checkDefaults(item, seen, conditional); err != nil {
			return err
		}
	}
	branches := append(append(append([]*jsonschema.Schema{}, s.AllOf...), s.AnyOf...), s.OneOf...)
	branches = append(branches, s.If, s.Then, s.Else, s.Not, s.RecursiveRef)
	if s.DynamicRef != nil {
		// Dynamic defaults cannot be chosen statically.
		branches = append(branches, s.DynamicRef.Ref)
	}
	for _, child := range s.DependentSchemas {
		branches = append(branches, child)
	}
	for _, child := range branches {
		if err := checkDefaults(child, seen, true); err != nil {
			return err
		}
	}
	return nil
}

func (s *Schema) Validate(value any) error { return s.compiled.Validate(value) }

func (s *Schema) ValidateJSON(raw []byte) error {
	value, err := Decode(raw)
	if err != nil {
		return err
	}
	return s.Validate(value)
}

// Prepare never coerces a value and never replaces explicit null. Defaults
// fill missing properties only. The caller's original arguments stay intact.
func (s *Schema) Prepare(args map[string]any) (map[string]any, error) {
	prepared := jsonvalue.Clone(args)
	if prepared == nil {
		prepared = map[string]any{}
	}
	if err := applyDefaults(s.compiled, prepared, 0); err != nil {
		return nil, err
	}
	if err := s.Validate(prepared); err != nil {
		return nil, err
	}
	return prepared, nil
}

func applyDefaults(s *jsonschema.Schema, value any, depth int) error {
	if s == nil {
		return nil
	}
	if depth > 128 {
		return fmt.Errorf("tool arguments/defaults exceed maximum schema depth")
	}
	if s.Ref != nil {
		if err := applyDefaults(s.Ref, value, depth+1); err != nil {
			return err
		}
	}
	switch value := value.(type) {
	case map[string]any:
		for key, child := range s.Properties {
			v, exists := value[key]
			if fallback := propertyDefault(child); !exists && fallback != nil {
				v = jsonvalue.Clone(*fallback)
				value[key] = v
				exists = true
			}
			if exists {
				if err := applyDefaults(child, v, depth+1); err != nil {
					return err
				}
			}
		}
		// Dynamic property schemas describe existing keys; they cannot invent
		// names. Patterns apply even when properties also defines the key.
		patterns := make([]jsonschema.Regexp, 0, len(s.PatternProperties))
		for pattern := range s.PatternProperties {
			patterns = append(patterns, pattern)
		}
		sort.Slice(patterns, func(i, j int) bool { return patterns[i].String() < patterns[j].String() })
		for key, v := range value {
			_, matched := s.Properties[key]
			for _, pattern := range patterns {
				if pattern.MatchString(key) {
					matched = true
					if err := applyDefaults(s.PatternProperties[pattern], v, depth+1); err != nil {
						return err
					}
				}
			}
			if !matched {
				if child, ok := s.AdditionalProperties.(*jsonschema.Schema); ok {
					if err := applyDefaults(child, v, depth+1); err != nil {
						return err
					}
				}
			}
		}
	case []any:
		for i, v := range value {
			item := s.Items2020
			if i < len(s.PrefixItems) {
				item = s.PrefixItems[i]
			} else if child, ok := s.Items.(*jsonschema.Schema); ok {
				item = child
			} else if children, ok := s.Items.([]*jsonschema.Schema); ok && i < len(children) {
				item = children[i]
			}
			if err := applyDefaults(item, v, depth+1); err != nil {
				return err
			}
		}
	}
	return nil
}

func propertyDefault(s *jsonschema.Schema) *any {
	seen := map[*jsonschema.Schema]bool{}
	for s != nil && !seen[s] {
		seen[s] = true
		if s.Default != nil {
			return s.Default
		}
		s = s.Ref
	}
	return nil
}

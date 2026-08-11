package datool

import (
	"encoding/json"
	"fmt"
	"strings"
)

// WithPropertyValue sets one JSON Schema keyword on a generated property.
// Property paths use encoding/json field names separated by dots.
func WithPropertyValue(path, keyword string, value any) Option {
	return WithTransformSchema(func(schema json.RawMessage) (json.RawMessage, error) {
		if strings.TrimSpace(keyword) == "" {
			return nil, fmt.Errorf("property schema keyword is required")
		}
		document, err := decodeSchemaDocument(schema)
		if err != nil {
			return nil, err
		}
		property, err := schemaProperty(document, path)
		if err != nil {
			return nil, err
		}
		copied, err := copySchemaValue(value)
		if err != nil {
			return nil, fmt.Errorf("property %q keyword %q: %w", path, keyword, err)
		}
		property[keyword] = copied
		return json.Marshal(document)
	})
}

// WithPropertyType replaces a generated property's type keyword. replacement
// may be a JSON Schema type string or a slice of type strings.
func WithPropertyType(path string, replacement any) Option {
	return WithTransformSchema(func(schema json.RawMessage) (json.RawMessage, error) {
		document, err := decodeSchemaDocument(schema)
		if err != nil {
			return nil, err
		}
		property, err := schemaProperty(document, path)
		if err != nil {
			return nil, err
		}
		copied, err := copySchemaValue(replacement)
		if err != nil {
			return nil, fmt.Errorf("property %q type: %w", path, err)
		}
		delete(property, "anyOf")
		delete(property, "oneOf")
		delete(property, "allOf")
		property["type"] = copied
		return json.Marshal(document)
	})
}

// WithPropertyEnum sets the allowed string values for a generated property.
func WithPropertyEnum(path string, values ...string) Option {
	return WithPropertyValue(path, "enum", append([]string(nil), values...))
}

// WithPropertySchema replaces a generated property's entire schema.
func WithPropertySchema(path string, replacement any) Option {
	return WithTransformSchema(func(schema json.RawMessage) (json.RawMessage, error) {
		document, err := decodeSchemaDocument(schema)
		if err != nil {
			return nil, err
		}
		properties, name, err := schemaPropertyParent(document, path)
		if err != nil {
			return nil, err
		}
		copied, err := copySchemaValue(replacement)
		if err != nil {
			return nil, fmt.Errorf("property %q replacement schema: %w", path, err)
		}
		if _, ok := copied.(map[string]any); !ok {
			return nil, fmt.Errorf("property %q replacement schema must be an object", path)
		}
		properties[name] = copied
		return json.Marshal(document)
	})
}

// WithoutProperty removes a generated property and its required marker.
func WithoutProperty(path string) Option {
	return WithTransformSchema(func(schema json.RawMessage) (json.RawMessage, error) {
		document, err := decodeSchemaDocument(schema)
		if err != nil {
			return nil, err
		}
		properties, name, err := schemaPropertyParent(document, path)
		if err != nil {
			return nil, err
		}
		delete(properties, name)
		parent, err := schemaObjectForPropertyParent(document, path)
		if err != nil {
			return nil, err
		}
		if required, ok := parent["required"].([]any); ok {
			kept := required[:0]
			for _, item := range required {
				if item != name {
					kept = append(kept, item)
				}
			}
			if len(kept) == 0 {
				delete(parent, "required")
			} else {
				parent["required"] = kept
			}
		}
		return json.Marshal(document)
	})
}

func decodeSchemaDocument(schema json.RawMessage) (map[string]any, error) {
	var document map[string]any
	if err := json.Unmarshal(schema, &document); err != nil {
		return nil, fmt.Errorf("decode generated schema: %w", err)
	}
	return document, nil
}

func schemaProperty(document map[string]any, path string) (map[string]any, error) {
	properties, name, err := schemaPropertyParent(document, path)
	if err != nil {
		return nil, err
	}
	property, ok := properties[name].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("generated schema property %q is not an object", path)
	}
	return property, nil
}

func schemaPropertyParent(document map[string]any, path string) (map[string]any, string, error) {
	parts, err := schemaPropertyPath(path)
	if err != nil {
		return nil, "", err
	}
	parent := document
	for _, part := range parts[:len(parts)-1] {
		properties, ok := schemaObjectProperties(parent)
		if !ok {
			return nil, "", fmt.Errorf("generated schema property %q has no object properties", part)
		}
		nested, ok := properties[part].(map[string]any)
		if !ok {
			return nil, "", fmt.Errorf("generated schema property %q is not an object", part)
		}
		parent = nested
	}
	properties, ok := schemaObjectProperties(parent)
	if !ok {
		return nil, "", fmt.Errorf("generated schema has no properties for %q", path)
	}
	name := parts[len(parts)-1]
	if _, exists := properties[name]; !exists {
		return nil, "", fmt.Errorf("generated schema has no property %q", path)
	}
	return properties, name, nil
}

func schemaObjectForPropertyParent(document map[string]any, path string) (map[string]any, error) {
	parts, err := schemaPropertyPath(path)
	if err != nil {
		return nil, err
	}
	parent := document
	for _, part := range parts[:len(parts)-1] {
		properties, ok := schemaObjectProperties(parent)
		if !ok {
			return nil, fmt.Errorf("generated schema property %q has no object properties", part)
		}
		nested, ok := properties[part].(map[string]any)
		if !ok {
			return nil, fmt.Errorf("generated schema property %q is not an object", part)
		}
		parent = nested
	}
	return schemaObjectBranch(parent)
}

func schemaObjectProperties(schema map[string]any) (map[string]any, bool) {
	object, err := schemaObjectBranch(schema)
	if err != nil {
		return nil, false
	}
	properties, ok := object["properties"].(map[string]any)
	return properties, ok
}

func schemaObjectBranch(schema map[string]any) (map[string]any, error) {
	if _, ok := schema["properties"].(map[string]any); ok {
		return schema, nil
	}
	if alternatives, ok := schema["anyOf"].([]any); ok {
		for _, alternative := range alternatives {
			candidate, ok := alternative.(map[string]any)
			if !ok {
				continue
			}
			if _, ok := candidate["properties"].(map[string]any); ok {
				return candidate, nil
			}
		}
	}
	return nil, fmt.Errorf("schema is not an object")
}

func schemaPropertyPath(path string) ([]string, error) {
	parts := strings.Split(path, ".")
	for _, part := range parts {
		if strings.TrimSpace(part) == "" {
			return nil, fmt.Errorf("property path %q is invalid", path)
		}
	}
	return parts, nil
}

func copySchemaValue(value any) (any, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	var copied any
	if err := json.Unmarshal(encoded, &copied); err != nil {
		return nil, err
	}
	return copied, nil
}

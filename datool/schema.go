package datool

import (
	"encoding"
	"encoding/json"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"time"
)

var (
	rawMessageType      = reflect.TypeFor[json.RawMessage]()
	byteSliceType       = reflect.TypeFor[[]byte]()
	timeType            = reflect.TypeFor[time.Time]()
	textUnmarshalerType = reflect.TypeFor[encoding.TextUnmarshaler]()
)

// Schema derives a JSON Schema object from T. T must be a struct or pointer to
// a struct. Exported fields use their encoding/json names and omitempty rules.
// Struct fields are closed by default with additionalProperties=false.
//
// Field descriptions can be supplied with a description tag. The jsonschema
// tag accepts required, optional, nullable, type, description, title, format,
// pattern, enum, default, example, minimum, maximum, exclusiveMinimum,
// exclusiveMaximum, multipleOf, minLength, maxLength, minItems, maxItems,
// minProperties, and maxProperties. Enum alternatives may be repeated or
// separated by |.
func Schema[T any]() (json.RawMessage, error) {
	typeOf := reflect.TypeFor[T]()
	for typeOf.Kind() == reflect.Pointer {
		typeOf = typeOf.Elem()
	}
	if typeOf.Kind() != reflect.Struct {
		return nil, fmt.Errorf("generate tool schema: input type %s must be a struct or pointer to a struct", typeOf)
	}
	document, err := schemaForType(typeOf, map[reflect.Type]bool{})
	if err != nil {
		return nil, fmt.Errorf("generate tool schema for %s: %w", typeOf, err)
	}
	encoded, err := json.Marshal(document)
	if err != nil {
		return nil, fmt.Errorf("encode tool schema for %s: %w", typeOf, err)
	}
	return encoded, nil
}

func schemaForType(typeOf reflect.Type, stack map[reflect.Type]bool) (map[string]any, error) {
	nullable := false
	for typeOf.Kind() == reflect.Pointer {
		nullable = true
		typeOf = typeOf.Elem()
	}

	var schema map[string]any
	switch {
	case typeOf == rawMessageType:
		schema = map[string]any{}
	case typeOf == byteSliceType:
		schema = map[string]any{"type": "string", "contentEncoding": "base64"}
	case typeOf == timeType:
		schema = map[string]any{"type": "string", "format": "date-time"}
	case typeOf.Implements(textUnmarshalerType) || reflect.PointerTo(typeOf).Implements(textUnmarshalerType):
		schema = map[string]any{"type": "string"}
	default:
		var err error
		switch typeOf.Kind() {
		case reflect.Bool:
			schema = map[string]any{"type": "boolean"}
		case reflect.String:
			schema = map[string]any{"type": "string"}
		case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
			reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
			schema = map[string]any{"type": "integer"}
		case reflect.Float32, reflect.Float64:
			schema = map[string]any{"type": "number"}
		case reflect.Slice, reflect.Array:
			var item map[string]any
			item, err = schemaForType(typeOf.Elem(), stack)
			if err == nil {
				schema = map[string]any{"type": "array", "items": item}
			}
		case reflect.Map:
			if typeOf.Key().Kind() != reflect.String {
				return nil, fmt.Errorf("map key type %s is not supported", typeOf.Key())
			}
			var value map[string]any
			value, err = schemaForType(typeOf.Elem(), stack)
			if err == nil {
				schema = map[string]any{"type": "object", "additionalProperties": value}
			}
		case reflect.Struct:
			schema, err = schemaForStruct(typeOf, stack)
		case reflect.Interface:
			schema = map[string]any{}
		default:
			err = fmt.Errorf("type %s is not supported", typeOf)
		}
		if err != nil {
			return nil, err
		}
	}

	if !nullable {
		return schema, nil
	}
	return map[string]any{"anyOf": []any{schema, map[string]any{"type": "null"}}}, nil
}

func schemaForStruct(typeOf reflect.Type, stack map[reflect.Type]bool) (map[string]any, error) {
	if stack[typeOf] {
		return nil, fmt.Errorf("recursive input type %s is not supported", typeOf)
	}
	stack[typeOf] = true
	defer delete(stack, typeOf)

	properties := map[string]any{}
	var required []string
	for index := range typeOf.NumField() {
		field := typeOf.Field(index)
		if field.PkgPath != "" {
			continue
		}
		name, omitEmpty, quoted, skip := jsonField(field)
		if skip {
			continue
		}
		if quoted {
			return nil, fmt.Errorf("field %s: encoding/json string option is not supported", field.Name)
		}

		if field.Anonymous && field.Tag.Get("json") == "" {
			embeddedType := field.Type
			for embeddedType.Kind() == reflect.Pointer {
				embeddedType = embeddedType.Elem()
			}
			if embeddedType.Kind() == reflect.Struct {
				embedded, err := schemaForStruct(embeddedType, stack)
				if err != nil {
					return nil, fmt.Errorf("field %s: %w", field.Name, err)
				}
				for key, value := range embedded["properties"].(map[string]any) {
					properties[key] = value
				}
				if field.Type.Kind() != reflect.Pointer && !omitEmpty {
					if values, ok := embedded["required"].([]string); ok {
						required = append(required, values...)
					}
				}
				continue
			}
		}

		property, err := schemaForType(field.Type, stack)
		if err != nil {
			return nil, fmt.Errorf("field %s: %w", field.Name, err)
		}
		requiredField, err := applySchemaTags(property, field, !omitEmpty)
		if err != nil {
			return nil, fmt.Errorf("field %s: %w", field.Name, err)
		}
		properties[name] = property
		if requiredField {
			required = append(required, name)
		}
	}

	schema := map[string]any{
		"type":                 "object",
		"properties":           properties,
		"additionalProperties": false,
	}
	if len(required) > 0 {
		schema["required"] = required
	}
	return schema, nil
}

func jsonField(field reflect.StructField) (name string, omitEmpty, quoted, skip bool) {
	parts := strings.Split(field.Tag.Get("json"), ",")
	if parts[0] == "-" {
		return "", false, false, true
	}
	name = parts[0]
	if name == "" {
		name = field.Name
	}
	for _, option := range parts[1:] {
		if option == "omitempty" || option == "omitzero" {
			omitEmpty = true
		}
		if option == "string" {
			quoted = true
		}
	}
	return name, omitEmpty, quoted, false
}

func applySchemaTags(schema map[string]any, field reflect.StructField, required bool) (bool, error) {
	if description := field.Tag.Get("description"); description != "" {
		schema["description"] = description
	}
	for _, directive := range strings.Split(field.Tag.Get("jsonschema"), ",") {
		directive = strings.TrimSpace(directive)
		if directive == "" {
			continue
		}
		key, value, hasValue := strings.Cut(directive, "=")
		key, value = strings.TrimSpace(key), strings.TrimSpace(value)
		switch key {
		case "required":
			required = true
		case "optional":
			required = false
		case "nullable":
			kind := field.Type.Kind()
			if kind != reflect.Pointer && kind != reflect.Map && kind != reflect.Slice && kind != reflect.Interface {
				return false, fmt.Errorf("nullable requires a pointer, map, slice, or interface field")
			}
			if _, alreadyNullable := schema["anyOf"]; !alreadyNullable {
				copy := cloneAnyMap(schema)
				clear(schema)
				schema["anyOf"] = []any{copy, map[string]any{"type": "null"}}
			}
		case "description", "title", "format", "pattern":
			if !hasValue {
				return false, fmt.Errorf("jsonschema directive %q requires a value", key)
			}
			schema[key] = value
		case "type":
			if !hasValue {
				return false, fmt.Errorf("jsonschema directive type requires a value")
			}
			values := strings.Split(value, "|")
			for index := range values {
				values[index] = strings.TrimSpace(values[index])
				switch values[index] {
				case "null", "boolean", "object", "array", "number", "integer", "string":
				default:
					return false, fmt.Errorf("unsupported JSON Schema type %q", values[index])
				}
			}
			delete(schema, "anyOf")
			if len(values) == 1 {
				schema["type"] = values[0]
			} else {
				schema["type"] = values
			}
		case "enum":
			if !hasValue {
				return false, fmt.Errorf("jsonschema directive enum requires a value")
			}
			values, _ := schema["enum"].([]any)
			for _, item := range strings.Split(value, "|") {
				parsed, err := parseTagScalar(field.Type, strings.TrimSpace(item))
				if err != nil {
					return false, fmt.Errorf("enum value %q: %w", item, err)
				}
				values = append(values, parsed)
			}
			schema["enum"] = values
		case "default", "example":
			if !hasValue {
				return false, fmt.Errorf("jsonschema directive %q requires a value", key)
			}
			parsed, err := parseTagScalar(field.Type, value)
			if err != nil {
				return false, fmt.Errorf("%s value %q: %w", key, value, err)
			}
			schema[key] = parsed
		case "minimum", "maximum", "exclusiveMinimum", "exclusiveMaximum", "multipleOf":
			if !hasValue {
				return false, fmt.Errorf("jsonschema directive %q requires a value", key)
			}
			number, err := strconv.ParseFloat(value, 64)
			if err != nil {
				return false, fmt.Errorf("%s must be numeric: %w", key, err)
			}
			schema[key] = number
		case "minLength", "maxLength", "minItems", "maxItems", "minProperties", "maxProperties":
			if !hasValue {
				return false, fmt.Errorf("jsonschema directive %q requires a value", key)
			}
			integer, err := strconv.ParseUint(value, 10, 64)
			if err != nil {
				return false, fmt.Errorf("%s must be a non-negative integer: %w", key, err)
			}
			schema[key] = integer
		default:
			return false, fmt.Errorf("unsupported jsonschema directive %q", key)
		}
	}
	return required, nil
}

func parseTagScalar(typeOf reflect.Type, value string) (any, error) {
	for typeOf.Kind() == reflect.Pointer {
		typeOf = typeOf.Elem()
	}
	switch typeOf.Kind() {
	case reflect.String:
		return value, nil
	case reflect.Bool:
		return strconv.ParseBool(value)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return strconv.ParseInt(value, 10, 64)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return strconv.ParseUint(value, 10, 64)
	case reflect.Float32, reflect.Float64:
		return strconv.ParseFloat(value, 64)
	default:
		var decoded any
		if err := json.Unmarshal([]byte(value), &decoded); err != nil {
			return nil, fmt.Errorf("value is not valid JSON for %s: %w", typeOf, err)
		}
		return decoded, nil
	}
}

func cloneAnyMap(source map[string]any) map[string]any {
	result := make(map[string]any, len(source))
	for key, value := range source {
		result[key] = value
	}
	return result
}

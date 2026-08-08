package checkpoint

import (
	"encoding/json"
	"reflect"
)

func cloneCheckpoint(value Checkpoint) Checkpoint {
	copy := value
	copy.ChannelValues = cloneStringAnyMap(value.ChannelValues)
	copy.ChannelVersions = cloneStringMap(value.ChannelVersions)
	if value.VersionsSeen != nil {
		copy.VersionsSeen = make(map[string]map[string]string, len(value.VersionsSeen))
		for key, versions := range value.VersionsSeen {
			copy.VersionsSeen[key] = cloneStringMap(versions)
		}
	}
	copy.UpdatedChannels = append([]string(nil), value.UpdatedChannels...)
	return copy
}

func cloneMetadata(value Metadata) Metadata {
	copy := value
	copy.Parents = cloneStringMap(value.Parents)
	if value.CountersSinceDeltaSnapshot != nil {
		copy.CountersSinceDeltaSnapshot = make(map[string]DeltaCounter, len(value.CountersSinceDeltaSnapshot))
		for key, counter := range value.CountersSinceDeltaSnapshot {
			copy.CountersSinceDeltaSnapshot[key] = counter
		}
	}
	copy.Extra = cloneStringAnyMap(value.Extra)
	return copy
}

func cloneStringMap(values map[string]string) map[string]string {
	if values == nil {
		return nil
	}
	result := make(map[string]string, len(values))
	for key, value := range values {
		result[key] = value
	}
	return result
}

func cloneStringAnyMap(values map[string]any) map[string]any {
	if values == nil {
		return nil
	}
	result := make(map[string]any, len(values))
	for key, value := range values {
		result[key] = cloneAny(value)
	}
	return result
}

func cloneAny(value any) any {
	if value == nil {
		return nil
	}
	return cloneReflect(reflect.ValueOf(value)).Interface()
}

func cloneReflect(value reflect.Value) reflect.Value {
	if !value.IsValid() {
		return value
	}
	switch value.Kind() {
	case reflect.Interface:
		if value.IsNil() {
			return reflect.Zero(value.Type())
		}
		copy := cloneReflect(value.Elem())
		wrapped := reflect.New(value.Type()).Elem()
		wrapped.Set(copy)
		return wrapped
	case reflect.Pointer:
		if value.IsNil() {
			return reflect.Zero(value.Type())
		}
		copy := reflect.New(value.Type().Elem())
		copy.Elem().Set(cloneReflect(value.Elem()))
		return copy
	case reflect.Slice:
		if value.IsNil() {
			return reflect.Zero(value.Type())
		}
		copy := reflect.MakeSlice(value.Type(), value.Len(), value.Len())
		for index := 0; index < value.Len(); index++ {
			copy.Index(index).Set(cloneReflect(value.Index(index)))
		}
		return copy
	case reflect.Array:
		copy := reflect.New(value.Type()).Elem()
		for index := 0; index < value.Len(); index++ {
			copy.Index(index).Set(cloneReflect(value.Index(index)))
		}
		return copy
	case reflect.Map:
		if value.IsNil() {
			return reflect.Zero(value.Type())
		}
		copy := reflect.MakeMapWithSize(value.Type(), value.Len())
		iterator := value.MapRange()
		for iterator.Next() {
			copy.SetMapIndex(cloneReflect(iterator.Key()), cloneReflect(iterator.Value()))
		}
		return copy
	case reflect.Struct:
		if value.Type() == reflect.TypeFor[json.RawMessage]() {
			return value
		}
		copy := reflect.New(value.Type()).Elem()
		for index := 0; index < value.NumField(); index++ {
			if copy.Field(index).CanSet() && value.Field(index).CanInterface() {
				copy.Field(index).Set(cloneReflect(value.Field(index)))
			} else {
				return value
			}
		}
		return copy
	default:
		return value
	}
}

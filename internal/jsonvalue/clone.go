// Package jsonvalue copies JSON-shaped values without encoding them, preserving
// numeric precision and named byte/slice types such as json.RawMessage.
package jsonvalue

import "reflect"

// Clone returns a recursively independent copy of maps, slices and arrays.
// Scalars retain their exact Go type. JSON values must be acyclic.
func Clone[T any](value T) T {
	copied := clone(reflect.ValueOf(value))
	if !copied.IsValid() {
		return value
	}
	return copied.Interface().(T)
}

func clone(v reflect.Value) reflect.Value {
	switch v.Kind() {
	case reflect.Interface:
		if v.IsNil() {
			return v
		}
		out := reflect.New(v.Type()).Elem()
		out.Set(clone(v.Elem()))
		return out
	case reflect.Map:
		if v.IsNil() {
			return v
		}
		out := reflect.MakeMapWithSize(v.Type(), v.Len())
		iter := v.MapRange()
		for iter.Next() {
			out.SetMapIndex(iter.Key(), clone(iter.Value()))
		}
		return out
	case reflect.Slice:
		if v.IsNil() {
			return v
		}
		out := reflect.MakeSlice(v.Type(), v.Len(), v.Len())
		for i := 0; i < v.Len(); i++ {
			out.Index(i).Set(clone(v.Index(i)))
		}
		return out
	case reflect.Array:
		out := reflect.New(v.Type()).Elem()
		for i := 0; i < v.Len(); i++ {
			out.Index(i).Set(clone(v.Index(i)))
		}
		return out
	default:
		return v
	}
}

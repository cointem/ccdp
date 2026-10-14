package session

import (
	"reflect"
)

// EventPointer gives reducers one representation for both newly constructed
// value facts and pointer facts read from a Store. Event is a sealed interface;
// every value implementation has the same pointer method set. This does not
// serialize data or reinterpret opaque provider content.
func EventPointer(event Event) Event {
	if event == nil {
		return nil
	}
	value := reflect.ValueOf(event)
	if value.Kind() == reflect.Pointer {
		if value.IsNil() {
			return nil
		}
		return event
	}
	pointer := reflect.New(value.Type())
	pointer.Elem().Set(value)
	return pointer.Interface().(Event)
}

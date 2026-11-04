package agent

import (
	"fmt"
	"reflect"
	"strings"
)

// describe names an event or an effect for the agent's own log: its type,
// then each of its fields that is a plain value, as name=value, and the
// type of each field that holds an interface, such as a command's event.
// Pointers, slices, maps and nested structs are left out, so a zone's
// tables or an outbox's entries don't flood the log, and so is anything
// whose type hides itself, such as a secret.
func describe(v any) string {
	var b strings.Builder
	b.WriteString(fmt.Sprintf("%T", v))
	rv := reflect.ValueOf(v)
	if rv.Kind() != reflect.Struct {
		return b.String()
	}
	t := rv.Type()
	for i := range t.NumField() {
		f, fv := t.Field(i), rv.Field(i)
		if !f.IsExported() {
			continue
		}
		var s string
		switch fv.Kind() {
		case reflect.String:
			if fv.Type() != reflect.TypeFor[string]() {
				continue
			}
			if fv.Len() == 0 {
				continue
			}
			s = fmt.Sprintf("%q", fv.String())
		case reflect.Bool:
			if !fv.Bool() {
				continue
			}
			s = "true"
		case reflect.Int, reflect.Int64:
			if fv.Int() == 0 {
				continue
			}
			s = fmt.Sprint(fv.Int())
		case reflect.Interface:
			if fv.IsNil() {
				continue
			}
			s = fmt.Sprintf("%T", fv.Interface())
		default:
			continue
		}
		b.WriteString(" " + strings.ToLower(f.Name) + "=" + s)
	}
	return b.String()
}

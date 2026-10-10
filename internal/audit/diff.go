package audit

import (
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strconv"
)

// Change operations.
const (
	OpAdd     = "add"
	OpRemove  = "remove"
	OpReplace = "replace"
)

// Change is one leaf-level difference between two documents.
type Change struct {
	Path string          `json:"path"`
	Op   string          `json:"op"`
	Old  json.RawMessage `json:"old,omitempty"`
	New  json.RawMessage `json:"new,omitempty"`
}

// DiffOptions tunes Diff.
type DiffOptions struct {
	// Ignore lists top-level fields that are excluded from the diff,
	// typically bookkeeping such as "version" and "updatedAt".
	Ignore []string
}

// Diff compares the JSON forms of before and after and returns the changes
// needed to turn one into the other. Either side may be nil, which models
// creation and deletion.
//
// Arrays whose elements are all objects carrying a unique "id" or "key"
// field are matched by that identity rather than by position. That way
// inserting a rule at the top of a list reports one added rule instead of
// every following rule as "replaced", and reordering is reported as a
// single change to the order. Other arrays are compared positionally.
func Diff(before, after any, opts DiffOptions) ([]Change, error) {
	a, err := toGeneric(before)
	if err != nil {
		return nil, fmt.Errorf("audit: encode before: %w", err)
	}
	b, err := toGeneric(after)
	if err != nil {
		return nil, fmt.Errorf("audit: encode after: %w", err)
	}
	if am, ok := a.(map[string]any); ok {
		for _, k := range opts.Ignore {
			delete(am, k)
		}
	}
	if bm, ok := b.(map[string]any); ok {
		for _, k := range opts.Ignore {
			delete(bm, k)
		}
	}
	d := &differ{}
	d.walk("$", a, b)
	return d.out, nil
}

func toGeneric(v any) (any, error) {
	if v == nil {
		return nil, nil
	}
	if rv := reflect.ValueOf(v); rv.Kind() == reflect.Pointer && rv.IsNil() {
		return nil, nil
	}
	raw, ok := v.(json.RawMessage)
	if !ok {
		var err error
		raw, err = json.Marshal(v)
		if err != nil {
			return nil, err
		}
	}
	if len(raw) == 0 {
		return nil, nil
	}
	var g any
	if err := json.Unmarshal(raw, &g); err != nil {
		return nil, err
	}
	return g, nil
}

type differ struct{ out []Change }

func encode(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	return b
}

func (d *differ) emit(path, op string, old, new any) {
	c := Change{Path: path, Op: op}
	if op != OpAdd {
		c.Old = encode(old)
	}
	if op != OpRemove {
		c.New = encode(new)
	}
	d.out = append(d.out, c)
}

func (d *differ) walk(path string, a, b any) {
	switch {
	case a == nil && b == nil:
		return
	case a == nil:
		d.emit(path, OpAdd, nil, b)
		return
	case b == nil:
		d.emit(path, OpRemove, a, nil)
		return
	}
	switch at := a.(type) {
	case map[string]any:
		bt, ok := b.(map[string]any)
		if !ok {
			d.emit(path, OpReplace, a, b)
			return
		}
		d.objects(path, at, bt)
	case []any:
		bt, ok := b.([]any)
		if !ok {
			d.emit(path, OpReplace, a, b)
			return
		}
		d.arrays(path, at, bt)
	default:
		if !reflect.DeepEqual(a, b) {
			d.emit(path, OpReplace, a, b)
		}
	}
}

func join(path, field string) string {
	if path == "$" {
		return field
	}
	return path + "." + field
}

func (d *differ) objects(path string, a, b map[string]any) {
	keys := make([]string, 0, len(a)+len(b))
	for k := range a {
		keys = append(keys, k)
	}
	for k := range b {
		if _, ok := a[k]; !ok {
			keys = append(keys, k)
		}
	}
	slices.Sort(keys)
	for _, k := range keys {
		av, aok := a[k]
		bv, bok := b[k]
		switch {
		case !aok:
			if bv != nil {
				d.emit(join(path, k), OpAdd, nil, bv)
			}
		case !bok:
			if av != nil {
				d.emit(join(path, k), OpRemove, av, nil)
			}
		default:
			d.walk(join(path, k), av, bv)
		}
	}
}

// identityField returns "id" or "key" when every element of every given
// array is an object with a unique string value for that field.
func identityField(arrays ...[]any) string {
	for _, field := range []string{"id", "key"} {
		ok := true
		for _, arr := range arrays {
			seen := map[string]bool{}
			for _, el := range arr {
				m, isObj := el.(map[string]any)
				if !isObj {
					ok = false
					break
				}
				s, isStr := m[field].(string)
				if !isStr || s == "" || seen[s] {
					ok = false
					break
				}
				seen[s] = true
			}
			if !ok {
				break
			}
		}
		if ok && (len(arrays[0]) > 0 || len(arrays[1]) > 0) {
			return field
		}
	}
	return ""
}

func (d *differ) arrays(path string, a, b []any) {
	if field := identityField(a, b); field != "" {
		d.keyedArrays(path, field, a, b)
		return
	}
	n := max(len(a), len(b))
	for i := 0; i < n; i++ {
		p := path + "[" + strconv.Itoa(i) + "]"
		switch {
		case i >= len(a):
			d.emit(p, OpAdd, nil, b[i])
		case i >= len(b):
			d.emit(p, OpRemove, a[i], nil)
		default:
			d.walk(p, a[i], b[i])
		}
	}
}

func (d *differ) keyedArrays(path, field string, a, b []any) {
	id := func(el any) string { return el.(map[string]any)[field].(string) }
	index := func(arr []any) map[string]any {
		m := make(map[string]any, len(arr))
		for _, el := range arr {
			m[id(el)] = el
		}
		return m
	}
	am, bm := index(a), index(b)
	elemPath := func(k string) string { return fmt.Sprintf("%s[%s=%s]", path, field, k) }

	for _, el := range a {
		k := id(el)
		if other, ok := bm[k]; ok {
			d.walk(elemPath(k), el, other)
		} else {
			d.emit(elemPath(k), OpRemove, el, nil)
		}
	}
	for _, el := range b {
		if _, ok := am[id(el)]; !ok {
			d.emit(elemPath(id(el)), OpAdd, nil, el)
		}
	}

	// Report reordering of the surviving elements as one change.
	var orderA, orderB []string
	for _, el := range a {
		if _, ok := bm[id(el)]; ok {
			orderA = append(orderA, id(el))
		}
	}
	for _, el := range b {
		if _, ok := am[id(el)]; ok {
			orderB = append(orderB, id(el))
		}
	}
	if !slices.Equal(orderA, orderB) {
		d.out = append(d.out, Change{Path: path + "[*]", Op: OpReplace, Old: encode(orderA), New: encode(orderB)})
	}
}

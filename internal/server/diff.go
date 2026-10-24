package server

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/thisisnabi/Nube/internal/api"
	"github.com/thisisnabi/Nube/internal/store"
)

// FieldChange is one changed field of an object in an apply or dry run.
type FieldChange struct {
	Path string `json:"path"`
	Old  any    `json:"old,omitempty"`
	New  any    `json:"new,omitempty"`
}

// ObjectChange describes what an apply did (or would do) to one object.
type ObjectChange struct {
	Object  string        `json:"object"` // kind/name
	NS      string        `json:"namespace,omitempty"`
	Action  string        `json:"action"` // created | configured | unchanged | deleted
	Changes []FieldChange `json:"changes,omitempty"`
}

// objectJSON returns the desired-state part of an object (spec, data,
// labels ...) as generic JSON, or nil when it does not exist.
func objectJSON(st *store.State, kind, ns, name string) map[string]any {
	key := ns + "/" + name
	if kind == api.KindCRD {
		key = "/" + name
	}
	o, ok := namespaced(st, kind)[key]
	if !ok {
		return nil
	}
	b, _ := json.Marshal(o)
	var m map[string]any
	json.Unmarshal(b, &m)
	delete(m, "status")
	delete(m, "kind")
	if meta, ok := m["metadata"].(map[string]any); ok {
		keep := map[string]any{}
		for _, k := range []string{"labels", "annotations"} {
			if v, ok := meta[k]; ok {
				keep[k] = v
			}
		}
		if len(keep) > 0 {
			m["metadata"] = keep
		} else {
			delete(m, "metadata")
		}
	}
	return m
}

// diffObjects lists changed leaf fields. Secret values are never shown.
func diffObjects(kind string, before, after map[string]any) []FieldChange {
	a, b := map[string]any{}, map[string]any{}
	flatten("", before, a)
	flatten("", after, b)
	var out []FieldChange
	for p, nv := range b {
		ov, had := a[p]
		if had && jsonEqual(ov, nv) {
			continue
		}
		c := FieldChange{Path: p, New: nv}
		if had {
			c.Old = ov
		}
		out = append(out, c)
	}
	for p, ov := range a {
		if _, ok := b[p]; !ok {
			out = append(out, FieldChange{Path: p, Old: ov})
		}
	}
	if kind == api.KindSecret {
		for i := range out {
			if strings.HasPrefix(out[i].Path, "data.") || strings.HasPrefix(out[i].Path, "stringData.") {
				if out[i].Old != nil {
					out[i].Old = "(hidden)"
				}
				if out[i].New != nil {
					out[i].New = "(hidden)"
				}
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

func flatten(prefix string, v any, out map[string]any) {
	switch t := v.(type) {
	case map[string]any:
		if len(t) == 0 && prefix != "" {
			return
		}
		for k, c := range t {
			p := k
			if prefix != "" {
				p = prefix + "." + k
			}
			flatten(p, c, out)
		}
	case []any:
		if len(t) == 0 {
			return
		}
		for i, c := range t {
			flatten(fmt.Sprintf("%s[%d]", prefix, i), c, out)
		}
	case nil:
	default:
		if prefix != "" {
			out[prefix] = t
		}
	}
}

package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/thisisnabi/Nube/internal/api"
	"github.com/thisisnabi/Nube/internal/store"
)

// LabelOwnedBy marks objects created by an operator for a custom object
// ("Kind/name"); they are deleted together with it.
const LabelOwnedBy = "nube/owned-by"

func applyCRD(st *store.State, o api.Object) (string, error) {
	var crd api.CustomResourceDefinition
	if err := json.Unmarshal(o.Raw, &crd); err != nil {
		return "", err
	}
	sp := &crd.Spec
	if !api.ValidCustomKind(sp.Kind) {
		return "", fmt.Errorf("crd/%s: spec.kind must be CamelCase letters and digits, e.g. Website", o.Name)
	}
	if sp.Plural == "" {
		sp.Plural = strings.ToLower(sp.Kind) + "s"
	}
	for _, n := range append([]string{sp.Kind, sp.Plural}, sp.ShortNames...) {
		if api.NormalizeKind(n) != "" {
			return "", fmt.Errorf("crd/%s: %q clashes with a built-in kind", o.Name, n)
		}
	}
	if sp.Schema.Type == "" {
		sp.Schema.Type = "object"
	}
	if sp.Schema.Type != "object" {
		return "", fmt.Errorf("crd/%s: schema must describe an object (the spec)", o.Name)
	}
	if err := checkSchema(&sp.Schema, "spec"); err != nil {
		return "", fmt.Errorf("crd/%s: %w", o.Name, err)
	}
	for _, other := range st.CRDs {
		if other.Metadata.Name != o.Name && (other.Spec.Kind == sp.Kind || other.Spec.Plural == sp.Plural) {
			return "", fmt.Errorf("crd/%s: kind %s is already defined by crd/%s", o.Name, sp.Kind, other.Metadata.Name)
		}
	}
	crd.Kind = api.KindCRD
	crd.Metadata.Name, crd.Metadata.Namespace = o.Name, ""
	if old := st.CRDs[sp.Kind]; old != nil {
		if jsonEqual(old.Spec, crd.Spec) {
			return "unchanged", nil
		}
		touchMeta(&crd.Metadata, &old.Metadata)
		crd.Metadata.Namespace = ""
		st.CRDs[sp.Kind] = &crd
		return "configured", nil
	}
	for k, c := range st.CRDs {
		if c.Metadata.Name == o.Name { // kind renamed
			delete(st.CRDs, k)
		}
	}
	touchMeta(&crd.Metadata, nil)
	crd.Metadata.Namespace = ""
	st.CRDs[sp.Kind] = &crd
	st.Event("Normal", api.KindCRD, "", o.Name, "Created", "custom resource "+sp.Kind+" ("+sp.Plural+") registered")
	return "created", nil
}

func checkSchema(s *api.SchemaProperty, path string) error {
	switch s.Type {
	case "string", "integer", "number", "boolean", "object", "array":
	default:
		return fmt.Errorf("%s: unsupported type %q", path, s.Type)
	}
	for name, p := range s.Properties {
		p := p
		if err := checkSchema(&p, path+"."+name); err != nil {
			return err
		}
	}
	if s.Items != nil {
		return checkSchema(s.Items, path+"[]")
	}
	return nil
}

// validateValue checks v against a schema, filling defaults, and returns it.
func validateValue(s *api.SchemaProperty, v any, path string) (any, error) {
	switch s.Type {
	case "object":
		m, ok := v.(map[string]any)
		if !ok {
			if v == nil {
				m = map[string]any{}
			} else {
				return nil, fmt.Errorf("%s must be an object", path)
			}
		}
		for name, p := range s.Properties {
			if _, ok := m[name]; !ok && len(p.Default) > 0 {
				var d any
				json.Unmarshal(p.Default, &d)
				m[name] = d
			}
		}
		for _, r := range s.Required {
			if _, ok := m[r]; !ok {
				return nil, fmt.Errorf("%s.%s is required", path, r)
			}
		}
		for k, val := range m {
			p, ok := s.Properties[k]
			if !ok {
				if len(s.Properties) > 0 {
					return nil, fmt.Errorf("%s.%s is not a known field", path, k)
				}
				continue // free-form object
			}
			nv, err := validateValue(&p, val, path+"."+k)
			if err != nil {
				return nil, err
			}
			m[k] = nv
		}
		return m, nil
	case "array":
		a, ok := v.([]any)
		if !ok {
			return nil, fmt.Errorf("%s must be an array", path)
		}
		if s.Items != nil {
			for i := range a {
				nv, err := validateValue(s.Items, a[i], fmt.Sprintf("%s[%d]", path, i))
				if err != nil {
					return nil, err
				}
				a[i] = nv
			}
		}
		return a, nil
	case "string":
		str, ok := v.(string)
		if !ok {
			return nil, fmt.Errorf("%s must be a string", path)
		}
		if len(s.Enum) > 0 && !contains(s.Enum, str) {
			return nil, fmt.Errorf("%s must be one of %s", path, strings.Join(s.Enum, ", "))
		}
		return str, nil
	case "boolean":
		if _, ok := v.(bool); !ok {
			return nil, fmt.Errorf("%s must be true or false", path)
		}
		return v, nil
	case "integer", "number":
		f, ok := v.(float64)
		if !ok {
			return nil, fmt.Errorf("%s must be a number", path)
		}
		if s.Type == "integer" && f != math.Trunc(f) {
			return nil, fmt.Errorf("%s must be an integer", path)
		}
		if s.Minimum != nil && f < *s.Minimum {
			return nil, fmt.Errorf("%s must be >= %v", path, *s.Minimum)
		}
		if s.Maximum != nil && f > *s.Maximum {
			return nil, fmt.Errorf("%s must be <= %v", path, *s.Maximum)
		}
		return f, nil
	}
	return v, nil
}

func applyCustom(st *store.State, o api.Object) (string, error) {
	crd := st.CRDs[o.Kind]
	if crd == nil {
		if c := st.CRDByName(o.Kind); c != nil {
			crd = c
		} else {
			return "", fmt.Errorf("unknown kind %q (no CustomResourceDefinition registers it)", o.Kind)
		}
	}
	var in struct {
		Metadata api.Meta        `json:"metadata"`
		Spec     json.RawMessage `json:"spec"`
	}
	if err := json.Unmarshal(o.Raw, &in); err != nil {
		return "", err
	}
	var spec any
	if len(in.Spec) > 0 {
		json.Unmarshal(in.Spec, &spec)
	}
	spec, err := validateValue(&crd.Spec.Schema, spec, "spec")
	if err != nil {
		return "", fmt.Errorf("%s/%s: %w", strings.ToLower(crd.Spec.Kind), o.Name, err)
	}
	specJSON, _ := json.Marshal(spec)
	key := store.CustomKey(crd.Spec.Kind, o.Namespace, o.Name)
	meta := in.Metadata
	meta.Name, meta.Namespace = o.Name, o.Namespace
	if old := st.Custom[key]; old != nil {
		if bytes.Equal(old.Spec, specJSON) && jsonEqual(old.Metadata.Labels, meta.Labels) {
			return "unchanged", nil
		}
		touchMeta(&meta, &old.Metadata)
		c := *old
		c.Metadata = meta
		if !bytes.Equal(old.Spec, specJSON) {
			c.Spec, c.Generation = specJSON, old.Generation+1
		}
		st.Revision++
		c.Version = st.Revision
		st.Custom[key] = &c
		return "configured", nil
	}
	touchMeta(&meta, nil)
	st.Revision++
	st.Custom[key] = &api.CustomObject{Kind: crd.Spec.Kind, Metadata: meta, Spec: specJSON, Generation: 1, Version: st.Revision}
	return "created", nil
}

// deleteCustom removes a custom object, the objects it owns, and leaves a
// tombstone so watchers learn about the deletion.
func deleteCustom(st *store.State, kind, ns, name string) error {
	key := store.CustomKey(kind, ns, name)
	o := st.Custom[key]
	if o == nil {
		return notFound(strings.ToLower(kind) + "/" + name)
	}
	delete(st.Custom, key)
	st.Revision++
	tomb := *o
	tomb.Version = st.Revision
	tomb.Metadata.Annotations = map[string]string{"nube/deleted": time.Now().UTC().Format(time.RFC3339)}
	st.Custom["~"+key] = &tomb
	pruneTombstones(st)
	collectOwned(st, ns, kind+"/"+name)
	st.Event("Normal", kind, ns, name, "Deleted", "custom object deleted with the objects it owns")
	return nil
}

func pruneTombstones(st *store.State) {
	var tombs []string
	for k := range st.Custom {
		if strings.HasPrefix(k, "~") {
			tombs = append(tombs, k)
		}
	}
	if len(tombs) <= 500 {
		return
	}
	sort.Slice(tombs, func(i, j int) bool { return st.Custom[tombs[i]].Version < st.Custom[tombs[j]].Version })
	for _, k := range tombs[:len(tombs)-500] {
		delete(st.Custom, k)
	}
}

// collectOwned garbage-collects objects labelled nube/owned-by=owner.
func collectOwned(st *store.State, ns, owner string) {
	owned := func(m api.Meta) bool { return m.Namespace == ns && m.Labels[LabelOwnedBy] == owner }
	for k, d := range st.Deployments {
		if owned(d.Metadata) {
			for _, p := range st.PodsOf(ns, d.Metadata.Name) {
				terminatePod(st, p, false)
			}
			delete(st.Deployments, k)
		}
	}
	for k, x := range st.StatefulSets {
		if owned(x.Metadata) {
			for _, p := range st.PodsOwned(ns, api.KindStatefulSet, x.Metadata.Name) {
				terminatePod(st, p, false)
			}
			delete(st.StatefulSets, k)
		}
	}
	for k, x := range st.Jobs {
		if owned(x.Metadata) {
			deleteJob(st, k, false)
		}
	}
	for k, x := range st.Services {
		if owned(x.Metadata) {
			delete(st.Services, k)
		}
	}
	for k, x := range st.Ingresses {
		if owned(x.Metadata) {
			delete(st.Ingresses, k)
		}
	}
	for k, x := range st.ConfigMaps {
		if owned(x.Metadata) {
			delete(st.ConfigMaps, k)
		}
	}
	for k, x := range st.Secrets {
		if owned(x.Metadata) {
			delete(st.Secrets, k)
		}
	}
}

func (s *Server) handleCustomStatus(w http.ResponseWriter, r *http.Request) {
	ns, plural, name := r.PathValue("ns"), r.PathValue("kind"), r.PathValue("name")
	var body struct {
		Status             json.RawMessage `json:"status"`
		ObservedGeneration int64           `json:"observedGeneration"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, 400, "body must be {\"status\": {...}, \"observedGeneration\": N}")
		return
	}
	var out *api.CustomObject
	err := s.store.Write(func(st *store.State) error {
		crd := st.CRDByName(plural)
		if crd == nil {
			return notFound("resource type " + plural)
		}
		key := store.CustomKey(crd.Spec.Kind, ns, name)
		o := st.Custom[key]
		if o == nil {
			return notFound(strings.ToLower(crd.Spec.Kind) + "/" + name)
		}
		c := *o
		if !bytes.Equal(c.Status, body.Status) || c.Observed != body.ObservedGeneration {
			c.Status, c.Observed = body.Status, body.ObservedGeneration
			st.Revision++
			c.Version = st.Revision
			st.Custom[key] = &c
		}
		out = &c
		return nil
	})
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	writeJSON(w, 200, out)
}

// handleWatch streams changes of one custom kind after a revision. It
// long-polls: the call returns as soon as there is something new (or after
// "timeout" seconds with an empty list).
func (s *Server) handleWatch(w http.ResponseWriter, r *http.Request) {
	plural := r.PathValue("kind")
	since, _ := strconv.ParseInt(r.URL.Query().Get("since"), 10, 64)
	timeout, _ := strconv.Atoi(r.URL.Query().Get("timeout"))
	if timeout <= 0 || timeout > 60 {
		timeout = 30
	}
	id := identityFrom(r)
	deadline := time.Now().Add(time.Duration(timeout) * time.Second)
	for {
		var events []api.WatchEvent
		var rev int64
		found := true
		s.store.Read(func(st *store.State) {
			rev = st.Revision
			crd := st.CRDByName(plural)
			if crd == nil {
				found = false
				return
			}
			prefix := crd.Spec.Kind + "|"
			for k, o := range st.Custom {
				deleted := strings.HasPrefix(k, "~")
				if !strings.HasPrefix(strings.TrimPrefix(k, "~"), prefix) || o.Version <= since || !nsAllowed(id, o.Metadata.Namespace) {
					continue
				}
				typ := "MODIFIED"
				switch {
				case deleted:
					typ = "DELETED"
				case o.Generation == 1 && o.Observed == 0 && o.Metadata.CreatedAt.Equal(o.Metadata.UpdatedAt):
					typ = "ADDED"
				}
				c := *o
				events = append(events, api.WatchEvent{Type: typ, Kind: o.Kind, Revision: o.Version, Object: &c})
			}
		})
		if !found {
			writeErr(w, 404, "unknown resource type "+plural)
			return
		}
		if len(events) > 0 || time.Now().After(deadline) || r.Context().Err() != nil {
			sort.Slice(events, func(i, j int) bool { return events[i].Revision < events[j].Revision })
			if events == nil {
				events = []api.WatchEvent{}
			}
			writeJSON(w, 200, map[string]any{"revision": rev, "events": events})
			return
		}
		time.Sleep(300 * time.Millisecond)
	}
}

// customList returns the custom objects of a kind (no tombstones).
func customList(st *store.State, kind string) map[string]any {
	out := map[string]any{}
	prefix := kind + "|"
	for k, o := range st.Custom {
		if strings.HasPrefix(k, prefix) {
			out[strings.TrimPrefix(k, prefix)] = o
		}
	}
	return out
}

// countCRDObjects updates each CRD's object count.
func countCRDObjects(st *store.State) {
	counts := map[string]int{}
	for k, o := range st.Custom {
		if !strings.HasPrefix(k, "~") {
			counts[o.Kind]++
		}
	}
	for _, c := range st.CRDs {
		c.Status.Objects = counts[c.Spec.Kind]
	}
}

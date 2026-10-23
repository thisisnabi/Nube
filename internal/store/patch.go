package store

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"

	"github.com/thisisnabi/Nube/internal/api"
)

// Patch is one replicated change: only the objects that changed, not the
// whole state. Collections are the State's map fields, by JSON name.
type Patch struct {
	Origin  string                                `json:"o,omitempty"` // server+sequence that produced it
	Sets    map[string]map[string]json.RawMessage `json:"s,omitempty"` // collection -> key -> object
	Dels    map[string][]string                   `json:"d,omitempty"` // collection -> keys
	Scalars map[string]json.RawMessage            `json:"v,omitempty"` // non-map fields
	Events  []api.Event                           `json:"e,omitempty"` // appended events
}

func (p *Patch) empty() bool {
	return len(p.Sets) == 0 && len(p.Dels) == 0 && len(p.Scalars) == 0 && len(p.Events) == 0
}

// committed mirrors the last committed state as JSON, per object. It is what
// writes are diffed against and what snapshots are made from.
type committed struct {
	maps    map[string]map[string][]byte
	scalars map[string][]byte
	events  []api.Event
}

type stateField struct {
	name  string // JSON name
	index int
	isMap bool
}

var stateFields = func() []stateField {
	t := reflect.TypeOf(State{})
	var out []stateField
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		name := strings.Split(f.Tag.Get("json"), ",")[0]
		if name == "" || name == "-" || name == "events" {
			continue
		}
		out = append(out, stateField{name: name, index: i, isMap: f.Type.Kind() == reflect.Map})
	}
	return out
}()

func newCommitted() *committed {
	return &committed{maps: map[string]map[string][]byte{}, scalars: map[string][]byte{}}
}

// snapshotOf fills a committed mirror from a state (used at start and on restore).
func snapshotOf(s *State) *committed {
	c := newCommitted()
	v := reflect.ValueOf(s).Elem()
	for _, f := range stateFields {
		fv := v.Field(f.index)
		if !f.isMap {
			b, _ := json.Marshal(fv.Interface())
			c.scalars[f.name] = b
			continue
		}
		m := map[string][]byte{}
		iter := fv.MapRange()
		for iter.Next() {
			b, _ := json.Marshal(iter.Value().Interface())
			m[iter.Key().String()] = b
		}
		c.maps[f.name] = m
	}
	c.events = append([]api.Event(nil), s.Events...)
	return c
}

// diff compares a (possibly modified) state with the committed mirror.
func diff(s *State, c *committed) *Patch {
	p := &Patch{}
	v := reflect.ValueOf(s).Elem()
	for _, f := range stateFields {
		fv := v.Field(f.index)
		if !f.isMap {
			b, _ := json.Marshal(fv.Interface())
			if !bytes.Equal(b, c.scalars[f.name]) {
				if p.Scalars == nil {
					p.Scalars = map[string]json.RawMessage{}
				}
				p.Scalars[f.name] = b
			}
			continue
		}
		old := c.maps[f.name]
		seen := map[string]bool{}
		iter := fv.MapRange()
		for iter.Next() {
			k := iter.Key().String()
			seen[k] = true
			b, _ := json.Marshal(iter.Value().Interface())
			if !bytes.Equal(b, old[k]) {
				if p.Sets == nil {
					p.Sets = map[string]map[string]json.RawMessage{}
				}
				if p.Sets[f.name] == nil {
					p.Sets[f.name] = map[string]json.RawMessage{}
				}
				p.Sets[f.name][k] = b
			}
		}
		for k := range old {
			if !seen[k] {
				if p.Dels == nil {
					p.Dels = map[string][]string{}
				}
				p.Dels[f.name] = append(p.Dels[f.name], k)
			}
		}
	}
	var last int64
	if n := len(c.events); n > 0 {
		last = c.events[n-1].ID
	}
	for _, e := range s.Events {
		if e.ID > last {
			p.Events = append(p.Events, e)
		}
	}
	return p
}

// apply applies a patch to a state (when it was produced elsewhere).
func (p *Patch) apply(s *State) error {
	v := reflect.ValueOf(s).Elem()
	for _, f := range stateFields {
		fv := v.Field(f.index)
		if !f.isMap {
			if b, ok := p.Scalars[f.name]; ok {
				ptr := reflect.New(fv.Type())
				if err := json.Unmarshal(b, ptr.Interface()); err != nil {
					return err
				}
				fv.Set(ptr.Elem())
			}
			continue
		}
		if fv.IsNil() {
			fv.Set(reflect.MakeMap(fv.Type()))
		}
		elem := fv.Type().Elem()
		for k, b := range p.Sets[f.name] {
			var val reflect.Value
			if elem.Kind() == reflect.Pointer {
				val = reflect.New(elem.Elem())
				if err := json.Unmarshal(b, val.Interface()); err != nil {
					return err
				}
			} else {
				ptr := reflect.New(elem)
				if err := json.Unmarshal(b, ptr.Interface()); err != nil {
					return err
				}
				val = ptr.Elem()
			}
			fv.SetMapIndex(reflect.ValueOf(k), val)
		}
		for _, k := range p.Dels[f.name] {
			fv.SetMapIndex(reflect.ValueOf(k), reflect.Value{})
		}
	}
	var last int64
	if n := len(s.Events); n > 0 {
		last = s.Events[n-1].ID
	}
	for _, e := range p.Events {
		if e.ID > last { // idempotent
			s.Events = append(s.Events, e)
		}
	}
	s.Events = trimEvents(s.Events)
	return nil
}

// record updates the committed mirror with a patch.
func (c *committed) record(p *Patch) {
	for name, sets := range p.Sets {
		if c.maps[name] == nil {
			c.maps[name] = map[string][]byte{}
		}
		for k, b := range sets {
			c.maps[name][k] = b
		}
	}
	for name, keys := range p.Dels {
		for _, k := range keys {
			delete(c.maps[name], k)
		}
	}
	for name, b := range p.Scalars {
		c.scalars[name] = b
	}
	var last int64
	if n := len(c.events); n > 0 {
		last = c.events[n-1].ID
	}
	for _, e := range p.Events {
		if e.ID > last {
			c.events = append(c.events, e)
		}
	}
	c.events = trimEvents(c.events)
}

// state rebuilds a State from the committed mirror (rollback / restore).
func (c *committed) state() (*State, error) {
	return decodeState(c.encode())
}

// encode serializes the committed mirror in the State JSON format.
func (c *committed) encode() []byte {
	var buf bytes.Buffer
	buf.WriteByte('{')
	first := true
	field := func(name string, raw []byte) {
		if !first {
			buf.WriteByte(',')
		}
		first = false
		nb, _ := json.Marshal(name)
		buf.Write(nb)
		buf.WriteByte(':')
		buf.Write(raw)
	}
	for _, f := range stateFields {
		if !f.isMap {
			if b, ok := c.scalars[f.name]; ok {
				field(f.name, b)
			}
			continue
		}
		var mb bytes.Buffer
		mb.WriteByte('{')
		i := 0
		for k, b := range c.maps[f.name] {
			if i > 0 {
				mb.WriteByte(',')
			}
			i++
			kb, _ := json.Marshal(k)
			mb.Write(kb)
			mb.WriteByte(':')
			mb.Write(b)
		}
		mb.WriteByte('}')
		field(f.name, mb.Bytes())
	}
	eb, _ := json.Marshal(c.events)
	field("events", eb)
	buf.WriteByte('}')
	return buf.Bytes()
}

func trimEvents(ev []api.Event) []api.Event {
	if len(ev) > maxEvents {
		return append([]api.Event(nil), ev[len(ev)-maxEvents:]...)
	}
	return ev
}

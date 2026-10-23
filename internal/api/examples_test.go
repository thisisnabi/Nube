package api

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// The shipped examples must parse, and their pod templates must validate.
func TestExamples(t *testing.T) {
	files, _ := filepath.Glob("../../examples/*.yaml")
	more, _ := filepath.Glob("../../examples/*/*.yaml")
	e2e, _ := filepath.Glob("../../test/e2e/*.yaml")
	files = append(append(files, more...), e2e...)
	if len(files) == 0 {
		t.Fatal("no examples found")
	}
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		objs, err := ParseManifests(data)
		if err != nil {
			t.Errorf("%s: %v", f, err)
			continue
		}
		for _, o := range objs {
			var probe struct {
				Spec struct {
					Template    *PodTemplate `json:"template"`
					JobTemplate *struct {
						Template *PodTemplate `json:"template"`
					} `json:"jobTemplate"`
				} `json:"spec"`
			}
			if err := json.Unmarshal(o.Raw, &probe); err != nil {
				continue // not a workload (e.g. a CRD schema)
			}
			tmpl := probe.Spec.Template
			if tmpl == nil && probe.Spec.JobTemplate != nil {
				tmpl = probe.Spec.JobTemplate.Template
			}
			if tmpl != nil {
				if err := tmpl.Validate(); err != nil {
					t.Errorf("%s: %s/%s: %v", f, o.Kind, o.Name, err)
				}
			}
		}
	}
}

package server

import (
	"net/http"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/thisisnabi/Nube/internal/api"
	"github.com/thisisnabi/Nube/internal/store"
)

// The schema endpoint describes every kind's fields (from the Go types) with
// an example, so people and AI agents can write manifests without guessing.

var kindTypes = map[string]reflect.Type{
	api.KindDeployment:     reflect.TypeOf(api.Deployment{}),
	api.KindStatefulSet:    reflect.TypeOf(api.StatefulSet{}),
	api.KindDaemonSet:      reflect.TypeOf(api.DaemonSet{}),
	api.KindJob:            reflect.TypeOf(api.Job{}),
	api.KindCronJob:        reflect.TypeOf(api.CronJob{}),
	api.KindPod:            reflect.TypeOf(api.Pod{}),
	api.KindService:        reflect.TypeOf(api.Service{}),
	api.KindIngress:        reflect.TypeOf(api.Ingress{}),
	api.KindConfigMap:      reflect.TypeOf(api.ConfigMap{}),
	api.KindSecret:         reflect.TypeOf(api.Secret{}),
	api.KindVolume:         reflect.TypeOf(api.Volume{}),
	api.KindVolumeSnapshot: reflect.TypeOf(api.VolumeSnapshot{}),
	api.KindNetworkPolicy:  reflect.TypeOf(api.NetworkPolicy{}),
	api.KindCRD:            reflect.TypeOf(api.CustomResourceDefinition{}),
	api.KindAgent:          reflect.TypeOf(api.Agent{}),
	api.KindAgentRun:       reflect.TypeOf(api.AgentRun{}),
}

var kindExamples = map[string]string{
	api.KindDeployment: `kind: Deployment
metadata: { name: web }
spec:
  replicas: 2
  template:
    image: nginx:1.27
    ports: [{ containerPort: 80 }]
    resources: { cpu: 100m, memory: 128Mi }
    envFrom: [{ configMap: web-config }]
    healthCheck: { httpGet: { path: /, port: 80 } }`,
	api.KindService: `kind: Service
metadata: { name: web }
spec:
  selector: { app: web }
  ports: [{ port: 8080, targetPort: 80 }]`,
	api.KindIngress: `kind: Ingress
metadata: { name: web }
spec:
  tls: [{ hosts: [app.example.com], acme: true }]
  rules:
    - host: app.example.com
      paths: [{ path: /, backend: { service: web, port: 8080 } }]`,
	api.KindConfigMap: `kind: ConfigMap
metadata: { name: web-config }
data: { MODE: production }`,
	api.KindSecret: `kind: Secret
metadata: { name: db }
stringData: { PASSWORD: change-me }`,
	api.KindVolume: `kind: Volume
metadata: { name: data }
spec: { type: local, size: 10Gi, backup: { schedule: "0 3 * * *", keep: 7 } }`,
	api.KindStatefulSet: `kind: StatefulSet
metadata: { name: db }
spec:
  replicas: 1
  serviceName: db
  volumeClaimTemplates: [{ name: data, size: 5Gi }]
  template:
    image: postgres:17-alpine
    volumes: [{ volume: data, containerPath: /var/lib/postgresql/data }]`,
	api.KindJob: `kind: Job
metadata: { name: migrate }
spec:
  template: { image: myapp, command: ["./migrate"], dependsOn: [db] }`,
	api.KindCronJob: `kind: CronJob
metadata: { name: report }
spec:
  schedule: "0 6 * * *"
  jobTemplate: { template: { image: myapp, command: ["./report"] } }`,
	api.KindDaemonSet: `kind: DaemonSet
metadata: { name: logger }
spec:
  template: { image: busybox:1.36, command: ["sh", "-c", "sleep infinity"] }`,
	api.KindNetworkPolicy: `kind: NetworkPolicy
metadata: { name: db-access }
spec:
  podSelector: { matchLabels: { app: db } }
  ingress: [{ from: [{ podSelector: { matchLabels: { app: api } } }], ports: [{ port: 5432 }] }]`,
	api.KindAgent: `kind: Agent
metadata: { name: support }
spec:
  model: { provider: anthropic, name: claude-opus-5-5, apiKeySecret: anthropic }
  instructions: You answer questions about our product using the knowledge base.
  tools:
    - name: search_kb
      description: Search the knowledge base
      http: { service: kb, port: 8080, path: /search }
      inputSchema: { type: object, properties: { query: { type: string } }, required: [query] }
    - name: shell
      sandbox: { image: python:3.12-slim }
      requireApproval: true
    - nube: { role: view }
  memory: { enabled: true }
  limits: { maxSteps: 20, timeoutSeconds: 600, concurrency: 4 }
  schedules: [{ cron: "0 9 * * 1-5", input: "Summarize yesterday's open tickets" }]`,
	api.KindAgentRun: `kind: AgentRun
metadata: { name: ask-1 }
spec: { agent: support, input: "How do I reset my password?", session: user-42 }`,
}

type schemaField struct {
	Name     string        `json:"name"`
	Type     string        `json:"type"`
	Fields   []schemaField `json:"fields,omitempty"`
	Optional bool          `json:"optional,omitempty"`
}

var timeType = reflect.TypeOf(time.Time{})

func fieldsOf(t reflect.Type, depth int) []schemaField {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct || depth > 8 {
		return nil
	}
	var out []schemaField
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if !f.IsExported() {
			continue
		}
		tag := f.Tag.Get("json")
		name, opts, _ := strings.Cut(tag, ",")
		if name == "-" {
			continue
		}
		if f.Anonymous && name == "" { // inlined (e.g. the single-container shorthand)
			out = append(out, fieldsOf(f.Type, depth)...)
			continue
		}
		if name == "" {
			name = f.Name
		}
		sf := schemaField{Name: name, Type: typeName(f.Type), Optional: strings.Contains(opts, "omitempty") || strings.Contains(opts, "omitzero")}
		el := f.Type
		for el.Kind() == reflect.Pointer || el.Kind() == reflect.Slice || el.Kind() == reflect.Map {
			el = el.Elem()
		}
		if el.Kind() == reflect.Struct && el != timeType {
			sf.Fields = fieldsOf(el, depth+1)
		}
		out = append(out, sf)
	}
	return out
}

func typeName(t reflect.Type) string {
	switch t.Kind() {
	case reflect.Pointer:
		return typeName(t.Elem())
	case reflect.Slice:
		if t.Elem().Kind() == reflect.Uint8 {
			return "bytes"
		}
		return "[]" + typeName(t.Elem())
	case reflect.Map:
		return "map[" + typeName(t.Key()) + "]" + typeName(t.Elem())
	case reflect.Struct:
		if t == timeType {
			return "time"
		}
		return "object"
	case reflect.Interface:
		return "any"
	case reflect.Int, reflect.Int64, reflect.Int32:
		return "integer"
	case reflect.Float64:
		return "number"
	}
	return t.Kind().String()
}

func (s *Server) handleSchemaList(w http.ResponseWriter, r *http.Request) {
	kinds := []map[string]any{}
	for k := range kindTypes {
		kinds = append(kinds, map[string]any{"kind": k, "plural": api.Plural(k), "hasExample": kindExamples[k] != ""})
	}
	sort.Slice(kinds, func(i, j int) bool { return kinds[i]["kind"].(string) < kinds[j]["kind"].(string) })
	s.store.Read(func(st *store.State) {
		for _, c := range st.CRDs {
			kinds = append(kinds, map[string]any{"kind": c.Spec.Kind, "plural": c.Spec.Plural, "custom": true})
		}
	})
	writeJSON(w, 200, map[string]any{"kinds": kinds})
}

func (s *Server) handleSchemaKind(w http.ResponseWriter, r *http.Request) {
	kind := s.resolveKind(r.PathValue("kind"))
	t, ok := kindTypes[kind]
	if !ok {
		var crd *api.CustomResourceDefinition
		s.store.Read(func(st *store.State) { crd = st.CRDs[kind] })
		if crd != nil {
			writeJSON(w, 200, map[string]any{"kind": kind, "custom": true, "schema": crd.Spec.Schema})
			return
		}
		writeErr(w, 404, "unknown kind "+r.PathValue("kind"))
		return
	}
	var fields []schemaField
	for _, f := range fieldsOf(t, 0) {
		if f.Name != "status" && f.Name != "kind" {
			fields = append(fields, f)
		}
	}
	writeJSON(w, 200, map[string]any{"kind": kind, "plural": api.Plural(kind), "fields": fields, "example": kindExamples[kind]})
}

package api

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// Object is a decoded manifest document: its kind plus the raw JSON body.
type Object struct {
	Kind      string          `json:"kind"`
	Name      string          `json:"name"`
	Namespace string          `json:"namespace"`
	Raw       json.RawMessage `json:"raw"`
}

var nameRE = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$`)

// ValidName reports whether s is a DNS-label style name.
func ValidName(s string) bool { return nameRE.MatchString(s) }

// ParseManifests decodes one or more YAML or JSON documents separated by "---".
// A top-level JSON array or a "kind: List" with "items" is also accepted.
func ParseManifests(data []byte) ([]Object, error) {
	data = bytes.TrimSpace(data)
	if len(data) == 0 {
		return nil, errors.New("empty manifest")
	}
	var docs []any
	if data[0] == '[' {
		if err := json.Unmarshal(data, &docs); err != nil {
			return nil, fmt.Errorf("invalid JSON array: %w", err)
		}
	} else {
		dec := yaml.NewDecoder(bytes.NewReader(data))
		for {
			var doc any
			err := dec.Decode(&doc)
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				return nil, fmt.Errorf("invalid YAML: %w", err)
			}
			if doc != nil {
				docs = append(docs, doc)
			}
		}
	}
	var out []Object
	for i, doc := range docs {
		m, ok := doc.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("document %d is not an object", i+1)
		}
		if k, _ := m["kind"].(string); strings.EqualFold(k, "List") {
			items, _ := m["items"].([]any)
			for _, it := range items {
				o, err := toObject(it)
				if err != nil {
					return nil, err
				}
				out = append(out, o)
			}
			continue
		}
		o, err := toObject(m)
		if err != nil {
			return nil, fmt.Errorf("document %d: %w", i+1, err)
		}
		out = append(out, o)
	}
	return out, nil
}

func toObject(doc any) (Object, error) {
	raw, err := json.Marshal(doc)
	if err != nil {
		return Object{}, err
	}
	var head struct {
		Kind     string `json:"kind"`
		Metadata Meta   `json:"metadata"`
	}
	if err := json.Unmarshal(raw, &head); err != nil {
		return Object{}, err
	}
	kind := NormalizeKind(head.Kind)
	if kind == "" {
		return Object{}, fmt.Errorf("unknown kind %q", head.Kind)
	}
	if !ValidName(head.Metadata.Name) {
		return Object{}, fmt.Errorf("%s: invalid metadata.name %q (lowercase letters, digits and '-')", kind, head.Metadata.Name)
	}
	ns := head.Metadata.Namespace
	if ns == "" {
		ns = DefaultNamespace
	}
	return Object{Kind: kind, Name: head.Metadata.Name, Namespace: ns, Raw: raw}, nil
}

// NormalizeKind maps user spellings ("deploy", "svc", "deployments") to a canonical kind.
func NormalizeKind(k string) string {
	switch strings.ToLower(k) {
	case "deployment", "deployments", "deploy":
		return KindDeployment
	case "pod", "pods", "po":
		return KindPod
	case "service", "services", "svc":
		return KindService
	case "configmap", "configmaps", "cm":
		return KindConfigMap
	case "secret", "secrets":
		return KindSecret
	case "volume", "volumes", "vol", "pv", "pvc":
		return KindVolume
	case "node", "nodes", "no":
		return KindNode
	case "job", "jobs":
		return KindJob
	case "cronjob", "cronjobs", "cj":
		return KindCronJob
	case "statefulset", "statefulsets", "sts":
		return KindStatefulSet
	case "ingress", "ingresses", "ing":
		return KindIngress
	case "token", "tokens":
		return KindToken
	}
	return ""
}

// Plural returns the URL path segment for a kind.
func Plural(kind string) string {
	switch kind {
	case KindDeployment:
		return "deployments"
	case KindPod:
		return "pods"
	case KindService:
		return "services"
	case KindConfigMap:
		return "configmaps"
	case KindSecret:
		return "secrets"
	case KindVolume:
		return "volumes"
	case KindNode:
		return "nodes"
	case KindJob:
		return "jobs"
	case KindCronJob:
		return "cronjobs"
	case KindStatefulSet:
		return "statefulsets"
	case KindIngress:
		return "ingresses"
	case KindToken:
		return "tokens"
	}
	return strings.ToLower(kind) + "s"
}

// ---------------------------------------------------------------- Quantities

// ParseCPU converts "250m", "0.5" or "2" into millicores.
func ParseCPU(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, nil
	}
	if strings.HasSuffix(s, "m") {
		v, err := strconv.ParseInt(strings.TrimSuffix(s, "m"), 10, 64)
		if err != nil || v < 0 {
			return 0, fmt.Errorf("invalid cpu %q", s)
		}
		return v, nil
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || f < 0 {
		return 0, fmt.Errorf("invalid cpu %q", s)
	}
	return int64(f * 1000), nil
}

// ParseMemory converts "128Mi", "1Gi", "512M", "1G" or a byte count into MiB.
func ParseMemory(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, nil
	}
	units := []struct {
		suffix string
		bytes  float64
	}{
		{"Ki", 1 << 10}, {"Mi", 1 << 20}, {"Gi", 1 << 30}, {"Ti", 1 << 40},
		{"K", 1e3}, {"M", 1e6}, {"G", 1e9}, {"T", 1e12},
	}
	mult := 1.0
	num := s
	for _, u := range units {
		if strings.HasSuffix(s, u.suffix) {
			mult = u.bytes
			num = strings.TrimSuffix(s, u.suffix)
			break
		}
	}
	f, err := strconv.ParseFloat(num, 64)
	if err != nil || f < 0 {
		return 0, fmt.Errorf("invalid memory %q", s)
	}
	mib := int64(f * mult / (1 << 20))
	if mib == 0 && f > 0 {
		mib = 1
	}
	return mib, nil
}

// Requests returns the pod's resource request: the sum of its app
// containers, or the largest init container if that is bigger (init
// containers run one at a time, before the app containers).
func (t *PodTemplate) Requests() Resources {
	var sum, initMax Resources
	for _, c := range t.AllContainers() {
		r := c.requests()
		sum.CPU += r.CPU
		sum.Memory += r.Memory
	}
	for _, c := range t.InitContainers {
		r := c.requests()
		initMax.CPU = max(initMax.CPU, r.CPU)
		initMax.Memory = max(initMax.Memory, r.Memory)
	}
	return Resources{CPU: max(sum.CPU, initMax.CPU), Memory: max(sum.Memory, initMax.Memory)}
}

func (c *Container) requests() Resources {
	cpu, _ := ParseCPU(c.Resources.CPU)
	mem, _ := ParseMemory(c.Resources.Memory)
	return Resources{CPU: cpu, Memory: mem}
}

// Hash returns a short, stable hash of the template; a change triggers a rollout.
func (t *PodTemplate) Hash() string {
	b, _ := json.Marshal(t)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])[:10]
}

// Validate checks a pod template for obvious mistakes.
func (t *PodTemplate) Validate() error {
	if len(t.Containers) > 0 && t.Image != "" {
		return errors.New("use either the inline image (single container) or containers, not both")
	}
	switch t.RestartPolicy {
	case "", "Always", "OnFailure", "Never":
	default:
		return fmt.Errorf("restartPolicy must be Always, OnFailure or Never")
	}
	emptyDirs := map[string]bool{}
	for _, e := range t.EmptyDirs {
		if !ValidName(e.Name) {
			return fmt.Errorf("emptyDir name %q is invalid", e.Name)
		}
		if e.Medium != "" && e.Medium != "Memory" {
			return fmt.Errorf("emptyDir %s: medium must be empty (disk) or Memory", e.Name)
		}
		if _, err := ParseMemory(e.SizeLimit); err != nil {
			return fmt.Errorf("emptyDir %s: %w", e.Name, err)
		}
		emptyDirs[e.Name] = true
	}
	names := map[string]bool{}
	hostPorts := map[string]bool{}
	check := func(c Container, what string) error {
		if !ValidName(c.Name) {
			return fmt.Errorf("%s name %q is invalid (lowercase letters, digits, '-')", what, c.Name)
		}
		if names[c.Name] {
			return fmt.Errorf("duplicate container name %q", c.Name)
		}
		names[c.Name] = true
		if strings.TrimSpace(c.Image) == "" {
			return fmt.Errorf("%s %s: image is required", what, c.Name)
		}
		if _, err := ParseCPU(c.Resources.CPU); err != nil {
			return fmt.Errorf("%s %s: %w", what, c.Name, err)
		}
		if _, err := ParseMemory(c.Resources.Memory); err != nil {
			return fmt.Errorf("%s %s: %w", what, c.Name, err)
		}
		switch c.ImagePullPolicy {
		case "", "Always", "IfNotPresent", "Never":
		default:
			return fmt.Errorf("imagePullPolicy must be Always, IfNotPresent or Never")
		}
		for _, p := range c.Ports {
			if p.ContainerPort <= 0 || p.ContainerPort > 65535 {
				return fmt.Errorf("%s %s: invalid containerPort %d", what, c.Name, p.ContainerPort)
			}
			if p.Protocol != "" && !strings.EqualFold(p.Protocol, "tcp") && !p.IsUDP() {
				return fmt.Errorf("%s %s: protocol must be TCP or UDP", what, c.Name)
			}
			k := fmt.Sprintf("%d/%v", p.ContainerPort, p.IsUDP())
			if hostPorts[k] {
				return fmt.Errorf("port %d is declared twice in the pod (containers share one network)", p.ContainerPort)
			}
			hostPorts[k] = true
		}
		for _, v := range c.Volumes {
			if v.ContainerPath == "" || !strings.HasPrefix(v.ContainerPath, "/") {
				return fmt.Errorf("%s %s: volume containerPath must be absolute", what, c.Name)
			}
			set := 0
			for _, s := range []string{v.Volume, v.EmptyDir, v.HostPath} {
				if s != "" {
					set++
				}
			}
			if set != 1 {
				return fmt.Errorf("%s %s: each volume mount needs exactly one of volume, emptyDir or hostPath", what, c.Name)
			}
		}
		return nil
	}
	for _, c := range t.InitContainers {
		if len(c.Ports) > 0 || c.HealthCheck != nil {
			return fmt.Errorf("init container %s cannot have ports or a healthCheck", c.Name)
		}
		if err := check(c, "init container"); err != nil {
			return err
		}
	}
	for _, c := range t.AllContainers() {
		if err := check(c, "container"); err != nil {
			return err
		}
	}
	return nil
}

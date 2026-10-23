package api

import (
	"encoding/json"
	"time"
)

const (
	KindDaemonSet      = "DaemonSet"
	KindNetworkPolicy  = "NetworkPolicy"
	KindCRD            = "CustomResourceDefinition"
	KindVolumeSnapshot = "VolumeSnapshot"
)

// ---------------------------------------------------------------- DaemonSet

type DaemonSetSpec struct {
	NodeSelector map[string]string `json:"nodeSelector,omitempty"`
	// MaxUnavailable is how many nodes may be updated at once during a rollout (default 1).
	MaxUnavailable int         `json:"maxUnavailable,omitempty"`
	Template       PodTemplate `json:"template"`
}

type DaemonSetStatus struct {
	DesiredNumber int    `json:"desiredNumberScheduled"`
	Current       int    `json:"currentNumberScheduled"`
	Ready         int    `json:"numberReady"`
	Updated       int    `json:"updatedNumberScheduled"`
	CurrentHash   string `json:"currentHash"`
	Phase         string `json:"phase"` // Healthy | Progressing | Degraded
	Message       string `json:"message,omitempty"`
}

// DaemonSet runs one pod on every node that matches its selector.
type DaemonSet struct {
	Kind     string          `json:"kind"`
	Metadata Meta            `json:"metadata"`
	Spec     DaemonSetSpec   `json:"spec"`
	Status   DaemonSetStatus `json:"status"`
}

// ---------------------------------------------------------------- NetworkPolicy

type LabelSelector struct {
	MatchLabels map[string]string `json:"matchLabels,omitempty"`
}

// Matches reports whether labels satisfy the selector (empty matches all).
func (s *LabelSelector) Matches(labels map[string]string) bool {
	if s == nil {
		return true
	}
	for k, v := range s.MatchLabels {
		if labels[k] != v {
			return false
		}
	}
	return true
}

type IPBlock struct {
	CIDR   string   `json:"cidr"`
	Except []string `json:"except,omitempty"`
}

// NetworkPolicyPeer selects traffic sources (ingress) or destinations (egress).
type NetworkPolicyPeer struct {
	PodSelector *LabelSelector `json:"podSelector,omitempty"`
	// Namespaces restricts PodSelector to these namespaces; empty = the policy's own namespace.
	// ["*"] means every namespace.
	Namespaces []string `json:"namespaces,omitempty"`
	IPBlock    *IPBlock `json:"ipBlock,omitempty"`
}

type NetworkPolicyPort struct {
	Protocol string `json:"protocol,omitempty"` // TCP (default) | UDP
	Port     int    `json:"port,omitempty"`     // 0 = all ports
}

type NetworkPolicyRule struct {
	// From (ingress) or To (egress); empty = any peer.
	From  []NetworkPolicyPeer `json:"from,omitempty"`
	To    []NetworkPolicyPeer `json:"to,omitempty"`
	Ports []NetworkPolicyPort `json:"ports,omitempty"` // empty = all ports
}

type NetworkPolicySpec struct {
	PodSelector LabelSelector       `json:"podSelector"`
	PolicyTypes []string            `json:"policyTypes,omitempty"` // Ingress, Egress (default: Ingress, plus Egress if egress rules exist)
	Ingress     []NetworkPolicyRule `json:"ingress,omitempty"`
	Egress      []NetworkPolicyRule `json:"egress,omitempty"`
}

// NetworkPolicy isolates the selected pods: only traffic allowed by some
// policy reaches (ingress) or leaves (egress) them, like Kubernetes.
type NetworkPolicy struct {
	Kind     string            `json:"kind"`
	Metadata Meta              `json:"metadata"`
	Spec     NetworkPolicySpec `json:"spec"`
	Status   struct {
		SelectedPods int `json:"selectedPods"`
	} `json:"status"`
}

// FirewallRule is one resolved allow rule sent to agents.
type FirewallRule struct {
	CIDRs []string            `json:"cidrs"` // empty = anywhere
	Ports []NetworkPolicyPort `json:"ports,omitempty"`
}

// PodFirewall is the effective policy of one local pod.
type PodFirewall struct {
	PodIP          string         `json:"podIP"`
	IsolateIngress bool           `json:"isolateIngress"`
	Ingress        []FirewallRule `json:"ingress,omitempty"`
	IsolateEgress  bool           `json:"isolateEgress"`
	Egress         []FirewallRule `json:"egress,omitempty"`
}

// ---------------------------------------------------------------- Custom resources

type SchemaProperty struct {
	Type        string                    `json:"type"` // string | integer | number | boolean | object | array
	Description string                    `json:"description,omitempty"`
	Enum        []string                  `json:"enum,omitempty"`
	Minimum     *float64                  `json:"minimum,omitempty"`
	Maximum     *float64                  `json:"maximum,omitempty"`
	Properties  map[string]SchemaProperty `json:"properties,omitempty"` // for objects
	Required    []string                  `json:"required,omitempty"`   // for objects
	Items       *SchemaProperty           `json:"items,omitempty"`      // for arrays
	Default     json.RawMessage           `json:"default,omitempty"`
}

type CRDSpec struct {
	Kind        string         `json:"kind"`             // e.g. Website
	Plural      string         `json:"plural,omitempty"` // e.g. websites (default: lower(kind)+"s")
	ShortNames  []string       `json:"shortNames,omitempty"`
	Description string         `json:"description,omitempty"`
	Schema      SchemaProperty `json:"schema"` // schema of .spec
	// PrinterColumns are .spec/.status fields shown by "nube get".
	PrinterColumns []PrinterColumn `json:"printerColumns,omitempty"`
}

type PrinterColumn struct {
	Name string `json:"name"`
	Path string `json:"path"` // e.g. spec.replicas, status.phase
}

// CustomResourceDefinition registers a new kind of object handled by an operator.
type CustomResourceDefinition struct {
	Kind     string  `json:"kind"`
	Metadata Meta    `json:"metadata"`
	Spec     CRDSpec `json:"spec"`
	Status   struct {
		Objects int `json:"objects"`
	} `json:"status"`
}

// CustomObject is an instance of a custom resource.
type CustomObject struct {
	Kind       string          `json:"kind"`
	Metadata   Meta            `json:"metadata"`
	Spec       json.RawMessage `json:"spec,omitempty"`
	Status     json.RawMessage `json:"status,omitempty"`
	Generation int64           `json:"generation"`         // bumped on every spec change
	Version    int64           `json:"resourceVersion"`    // bumped on every change
	Observed   int64           `json:"observedGeneration"` // set by the operator via the status API
}

// WatchEvent is one change in the watch stream.
type WatchEvent struct {
	Type     string `json:"type"` // ADDED | MODIFIED | DELETED
	Kind     string `json:"kind"`
	Revision int64  `json:"revision"`
	Object   any    `json:"object"`
}

// ---------------------------------------------------------------- Snapshots & backups

type VolumeSnapshotSpec struct {
	Volume string `json:"volume"`
}

type VolumeSnapshotStatus struct {
	Phase     string    `json:"phase"` // Pending | Uploading | Ready | Failed
	SizeBytes int64     `json:"sizeBytes"`
	SHA256    string    `json:"sha256,omitempty"`
	TakenAt   time.Time `json:"takenAt,omitempty"`
	Node      string    `json:"node,omitempty"`     // where it was taken
	StoredOn  []string  `json:"storedOn,omitempty"` // control-plane servers holding a copy
	Message   string    `json:"message,omitempty"`
}

// VolumeSnapshot is a point-in-time copy of a volume, stored on the control
// plane (off the node), usable for restore, migration and disaster recovery.
type VolumeSnapshot struct {
	Kind     string               `json:"kind"`
	Metadata Meta                 `json:"metadata"`
	Spec     VolumeSnapshotSpec   `json:"spec"`
	Status   VolumeSnapshotStatus `json:"status"`
}

// VolumeBackup schedules automatic snapshots of a volume.
type VolumeBackup struct {
	Schedule string `json:"schedule"`       // cron, e.g. "0 3 * * *"
	Keep     int    `json:"keep,omitempty"` // snapshots to keep (default 7)
}

// SnapshotTask asks an agent to snapshot or restore a volume.
type SnapshotTask struct {
	Namespace string `json:"namespace"`
	Snapshot  string `json:"snapshot"`
	Volume    Volume `json:"volume"`
}

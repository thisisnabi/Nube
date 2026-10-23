// Package api defines the resource types shared by the server, agent and CLI.
package api

import (
	"fmt"
	"strings"
	"time"
)

const (
	KindNode        = "Node"
	KindDeployment  = "Deployment"
	KindPod         = "Pod"
	KindService     = "Service"
	KindConfigMap   = "ConfigMap"
	KindSecret      = "Secret"
	KindVolume      = "Volume"
	KindJob         = "Job"
	KindCronJob     = "CronJob"
	KindStatefulSet = "StatefulSet"
	KindIngress     = "Ingress"
	KindToken       = "Token"

	DefaultNamespace = "default"
)

// Meta is the common metadata carried by every namespaced object.
type Meta struct {
	Name        string            `json:"name"`
	Namespace   string            `json:"namespace,omitempty"`
	UID         string            `json:"uid,omitempty"`
	Labels      map[string]string `json:"labels,omitempty"`
	Annotations map[string]string `json:"annotations,omitempty"`
	CreatedAt   time.Time         `json:"createdAt"`
	UpdatedAt   time.Time         `json:"updatedAt"`
}

// Key returns the store key "namespace/name".
func (m Meta) Key() string { return m.Namespace + "/" + m.Name }

// ---------------------------------------------------------------- Node

type NodeStatus string

const (
	NodeReady    NodeStatus = "Ready"
	NodeNotReady NodeStatus = "NotReady"
)

type Resources struct {
	CPU    int64 `json:"cpu"`    // millicores
	Memory int64 `json:"memory"` // MiB
}

type NodeInfo struct {
	OS            string `json:"os,omitempty"`
	Kernel        string `json:"kernel,omitempty"`
	Arch          string `json:"arch,omitempty"`
	RuntimeVer    string `json:"runtimeVersion,omitempty"`
	Runtime       string `json:"runtime,omitempty"`
	AgentVersion  string `json:"agentVersion,omitempty"`
	ContainerHost string `json:"containerHost,omitempty"`
}

type Node struct {
	Name          string            `json:"name"`
	Address       string            `json:"address"` // agent API host:port
	HostIP        string            `json:"hostIP"`  // where published container ports are reachable
	Labels        map[string]string `json:"labels,omitempty"`
	Status        NodeStatus        `json:"status"`
	Cordoned      bool              `json:"cordoned"`
	Capacity      Resources         `json:"capacity"`
	Allocated     Resources         `json:"allocated"` // sum of pod requests
	Usage         Resources         `json:"usage"`     // live usage reported by agent
	Info          NodeInfo          `json:"info"`
	PodCount      int               `json:"podCount"`
	PodCIDR       string            `json:"podCIDR,omitempty"`     // e.g. 10.88.3.0/24, allocated by the server
	NetworkMode   string            `json:"networkMode,omitempty"` // none | host-gw | wireguard
	WGPublicKey   string            `json:"wgPublicKey,omitempty"`
	WGPort        int               `json:"wgPort,omitempty"`
	NetworkReady  bool              `json:"networkReady"`
	NetworkError  string            `json:"networkError,omitempty"`
	TLS           bool              `json:"tls"` // agent API is served with a cluster-signed certificate
	LastHeartbeat time.Time         `json:"lastHeartbeat"`
	JoinedAt      time.Time         `json:"joinedAt"`
}

// ---------------------------------------------------------------- Workloads

type ContainerPort struct {
	Name          string `json:"name,omitempty"`
	ContainerPort int    `json:"containerPort"`
	HostPort      int    `json:"hostPort,omitempty"` // 0 = random
	Protocol      string `json:"protocol,omitempty"` // TCP (default) or UDP
}

// IsUDP reports whether the port is UDP.
func (p ContainerPort) IsUDP() bool { return strings.EqualFold(p.Protocol, "udp") }

type ResourceRequest struct {
	CPU    string `json:"cpu,omitempty"`    // "250m", "0.5", "2"
	Memory string `json:"memory,omitempty"` // "128Mi", "1Gi"
}

type EnvFromSource struct {
	ConfigMap string `json:"configMap,omitempty"`
	Secret    string `json:"secret,omitempty"`
}

type HTTPGetProbe struct {
	Path string `json:"path,omitempty"`
	Port int    `json:"port"`
}

type TCPProbe struct {
	Port int `json:"port"`
}

type HealthCheck struct {
	HTTPGet             *HTTPGetProbe `json:"httpGet,omitempty"`
	TCPSocket           *TCPProbe     `json:"tcpSocket,omitempty"`
	InitialDelaySeconds int           `json:"initialDelaySeconds,omitempty"`
	PeriodSeconds       int           `json:"periodSeconds,omitempty"`
	TimeoutSeconds      int           `json:"timeoutSeconds,omitempty"`
	FailureThreshold    int           `json:"failureThreshold,omitempty"`
}

// VolumeMount mounts a cluster Volume, a pod-scoped emptyDir or a host directory.
type VolumeMount struct {
	Volume        string `json:"volume,omitempty"`   // Volume object (or StatefulSet volumeClaimTemplate) name
	EmptyDir      string `json:"emptyDir,omitempty"` // scratch volume shared by the pod's containers
	HostPath      string `json:"hostPath,omitempty"`
	ContainerPath string `json:"containerPath"`
	ReadOnly      bool   `json:"readOnly,omitempty"`
}

// Container is one process of a pod.
type Container struct {
	Name            string            `json:"name,omitempty"`
	Image           string            `json:"image,omitempty"`
	ImagePullPolicy string            `json:"imagePullPolicy,omitempty"` // Always | IfNotPresent | Never
	Command         []string          `json:"command,omitempty"`
	Args            []string          `json:"args,omitempty"`
	Env             map[string]string `json:"env,omitempty"`
	EnvFrom         []EnvFromSource   `json:"envFrom,omitempty"`
	Ports           []ContainerPort   `json:"ports,omitempty"`
	Resources       ResourceRequest   `json:"resources,omitempty"`
	HealthCheck     *HealthCheck      `json:"healthCheck,omitempty"`
	Volumes         []VolumeMount     `json:"volumes,omitempty"`
}

// EmptyDir is a pod-scoped scratch volume, deleted with the pod.
type EmptyDir struct {
	Name      string `json:"name"`
	Medium    string `json:"medium,omitempty"`    // "" (disk) or "Memory" (tmpfs)
	SizeLimit string `json:"sizeLimit,omitempty"` // e.g. "64Mi"; enforced
}

// PodTemplate describes the pod every replica runs. A single container can be
// written inline (image, command, ports ... at the top level); several
// containers go in "containers" and share one network namespace (localhost)
// and any emptyDirs. initContainers run to completion, in order, first.
type PodTemplate struct {
	Labels         map[string]string `json:"labels,omitempty"`
	Container                        // single-container shorthand (fields are inlined)
	Containers     []Container       `json:"containers,omitempty"`
	InitContainers []Container       `json:"initContainers,omitempty"`
	EmptyDirs      []EmptyDir        `json:"emptyDirs,omitempty"`
	RestartPolicy  string            `json:"restartPolicy,omitempty"` // Always | OnFailure | Never
	Annotations    map[string]string `json:"annotations,omitempty"`
}

// AllContainers returns the app containers, turning the inline shorthand into
// a container named "main".
func (t *PodTemplate) AllContainers() []Container {
	if len(t.Containers) > 0 {
		return t.Containers
	}
	c := t.Container
	if c.Name == "" {
		c.Name = "main"
	}
	return []Container{c}
}

// AllPorts returns the ports of every app container.
func (t *PodTemplate) AllPorts() []ContainerPort {
	var out []ContainerPort
	for _, c := range t.AllContainers() {
		out = append(out, c.Ports...)
	}
	return out
}

// AllVolumeMounts returns the mounts of every app and init container.
func (t *PodTemplate) AllVolumeMounts() []VolumeMount {
	var out []VolumeMount
	for _, c := range append(t.AllContainers(), t.InitContainers...) {
		out = append(out, c.Volumes...)
	}
	return out
}

// MainImage is the image of the first app container (for display).
func (t *PodTemplate) MainImage() string {
	cs := t.AllContainers()
	if len(cs) > 1 {
		return fmt.Sprintf("%s (+%d)", cs[0].Image, len(cs)-1)
	}
	return cs[0].Image
}

type Strategy struct {
	MaxSurge       int `json:"maxSurge"`
	MaxUnavailable int `json:"maxUnavailable"`
}

type Autoscale struct {
	Min              int `json:"min"`
	Max              int `json:"max"`
	TargetCPUPercent int `json:"targetCPUPercent"`
}

type DeploymentSpec struct {
	Replicas     int               `json:"replicas"`
	Strategy     *Strategy         `json:"strategy,omitempty"`
	NodeSelector map[string]string `json:"nodeSelector,omitempty"`
	Autoscale    *Autoscale        `json:"autoscale,omitempty"`
	Paused       bool              `json:"paused,omitempty"`
	Template     PodTemplate       `json:"template"`
}

type Revision struct {
	Revision  int         `json:"revision"`
	Hash      string      `json:"hash"`
	Template  PodTemplate `json:"template"`
	CreatedAt time.Time   `json:"createdAt"`
}

type DeploymentStatus struct {
	Replicas          int        `json:"replicas"`
	ReadyReplicas     int        `json:"readyReplicas"`
	UpdatedReplicas   int        `json:"updatedReplicas"`
	AvailableReplicas int        `json:"availableReplicas"`
	Revision          int        `json:"revision"`
	CurrentHash       string     `json:"currentHash"`
	Phase             string     `json:"phase"` // Healthy | Progressing | Degraded | Paused
	Message           string     `json:"message,omitempty"`
	History           []Revision `json:"history,omitempty"`
	LastScaleTime     time.Time  `json:"lastScaleTime,omitempty"`
	CPUPercent        int        `json:"cpuPercent"`
}

type Deployment struct {
	Kind     string           `json:"kind"`
	Metadata Meta             `json:"metadata"`
	Spec     DeploymentSpec   `json:"spec"`
	Status   DeploymentStatus `json:"status"`
}

type PodPhase string

const (
	PodPending     PodPhase = "Pending"     // waiting for scheduling or a dependency
	PodScheduled   PodPhase = "Scheduled"   // assigned, agent hasn't acted yet
	PodPulling     PodPhase = "Pulling"     // image pull in progress
	PodInit        PodPhase = "Init"        // init containers running
	PodStarting    PodPhase = "Starting"    // containers created / waiting for readiness
	PodRunning     PodPhase = "Running"     // containers running
	PodCrashLoop   PodPhase = "CrashLoop"   // a container keeps exiting
	PodSucceeded   PodPhase = "Succeeded"   // all containers exited 0 (restartPolicy != Always)
	PodFailed      PodPhase = "Failed"      // a container failed and will not be restarted
	PodTerminating PodPhase = "Terminating" // deletion requested
	PodTerminated  PodPhase = "Terminated"  // reported by agent once the containers are gone
)

type PodSpec struct {
	Owner        string            `json:"owner,omitempty"`     // name of the owning workload
	OwnerKind    string            `json:"ownerKind,omitempty"` // Deployment | Job | StatefulSet ("" = Deployment)
	Subdomain    string            `json:"subdomain,omitempty"` // StatefulSet service: <pod>.<subdomain>.<ns>.svc DNS
	TemplateHash string            `json:"templateHash,omitempty"`
	NodeName     string            `json:"nodeName,omitempty"`
	NodeSelector map[string]string `json:"nodeSelector,omitempty"`
	Template     PodTemplate       `json:"template"`
}

// ContainerStatus is the state of one container inside a pod.
type ContainerStatus struct {
	Name        string `json:"name"`
	Init        bool   `json:"init,omitempty"`
	State       string `json:"state"` // Waiting | Running | Terminated
	Ready       bool   `json:"ready"`
	Restarts    int    `json:"restarts"`
	ExitCode    int    `json:"exitCode"`
	Message     string `json:"message,omitempty"`
	ContainerID string `json:"containerID,omitempty"`
	Image       string `json:"image,omitempty"`
}

type PodStatus struct {
	Phase        PodPhase          `json:"phase"`
	Ready        bool              `json:"ready"`
	Message      string            `json:"message,omitempty"`
	ContainerID  string            `json:"containerID,omitempty"` // first app container (default for logs)
	Containers   []ContainerStatus `json:"containers,omitempty"`
	HostIP       string            `json:"hostIP,omitempty"`
	PodIP        string            `json:"podIP,omitempty"`
	HostPorts    map[int]int       `json:"hostPorts,omitempty"`    // TCP containerPort -> hostPort
	HostPortsUDP map[int]int       `json:"hostPortsUDP,omitempty"` // UDP containerPort -> hostPort
	Restarts     int               `json:"restarts"`
	ExitCode     int               `json:"exitCode"`
	CPU          int64             `json:"cpu"`    // millicores in use
	Memory       int64             `json:"memory"` // MiB in use
	StartedAt    time.Time         `json:"startedAt,omitempty"`
	ScheduledAt  time.Time         `json:"scheduledAt,omitempty"`
	FinishedAt   time.Time         `json:"finishedAt,omitempty"`
	DeletionAt   time.Time         `json:"deletionAt,omitempty"`
}

type Pod struct {
	Kind     string    `json:"kind"`
	Metadata Meta      `json:"metadata"`
	Spec     PodSpec   `json:"spec"`
	Status   PodStatus `json:"status"`
}

// OwnedBy reports whether the pod belongs to the given workload.
func (p *Pod) OwnedBy(kind, name string) bool {
	k := p.Spec.OwnerKind
	if k == "" && p.Spec.Owner != "" {
		k = KindDeployment
	}
	return p.Spec.Owner == name && k == kind
}

// Finished reports whether the pod ran to completion (successfully or not).
func (p *Pod) Finished() bool {
	return p.Status.Phase == PodSucceeded || p.Status.Phase == PodFailed
}

// ---------------------------------------------------------------- Services

type ServicePort struct {
	Name       string `json:"name,omitempty"`
	Port       int    `json:"port"`               // port the cluster load balancer listens on
	TargetPort int    `json:"targetPort"`         // container port
	Protocol   string `json:"protocol,omitempty"` // TCP (default) | UDP
}

// IsUDP reports whether the port is UDP.
func (p ServicePort) IsUDP() bool { return strings.EqualFold(p.Protocol, "udp") }

type ServiceSpec struct {
	Selector map[string]string `json:"selector"`
	Ports    []ServicePort     `json:"ports"`
	// Headless services resolve to the pod IPs directly (no proxy); StatefulSets
	// use one for stable per-pod DNS names.
	Headless bool `json:"headless,omitempty"`
	// Internal services are reachable inside the cluster only (no port on the servers).
	Internal bool `json:"internal,omitempty"`
}

type ServiceStatus struct {
	Endpoints []string `json:"endpoints,omitempty"` // host:port of ready pods (first port)
	Message   string   `json:"message,omitempty"`
	Listening bool     `json:"listening"`
	Conns     int64    `json:"connections"`
}

type Service struct {
	Kind     string        `json:"kind"`
	Metadata Meta          `json:"metadata"`
	Spec     ServiceSpec   `json:"spec"`
	Status   ServiceStatus `json:"status"`
}

// ---------------------------------------------------------------- Config

type ConfigMap struct {
	Kind     string            `json:"kind"`
	Metadata Meta              `json:"metadata"`
	Data     map[string]string `json:"data"`
}

type Secret struct {
	Kind     string            `json:"kind"`
	Metadata Meta              `json:"metadata"`
	Type     string            `json:"type,omitempty"` // Opaque (default) | tls (keys tls.crt, tls.key)
	Data     map[string]string `json:"data"`           // plain values; masked on read unless ?reveal=1
}

// ---------------------------------------------------------------- Storage

type NFSSource struct {
	Server  string `json:"server"`
	Path    string `json:"path"`
	Options string `json:"options,omitempty"` // extra mount options, e.g. "nfsvers=4.1"
}

type VolumeSpec struct {
	// Type is "local" (on one node; pods follow it), "nfs" (network share
	// usable from any node) or "hostPath" (same path on every node).
	Type string `json:"type"`
	// Size is enforced for local volumes: the volume is its own ext4 filesystem
	// of exactly this size, so writes beyond it fail with "no space left".
	Size     string     `json:"size,omitempty"`
	NodeName string     `json:"nodeName,omitempty"`
	NFS      *NFSSource `json:"nfs,omitempty"`
	HostPath string     `json:"hostPath,omitempty"`
}

type VolumeStatus struct {
	Phase       string   `json:"phase"` // Available | Bound
	NodeName    string   `json:"nodeName,omitempty"`
	UsedBy      []string `json:"usedBy,omitempty"`
	UsedMiB     int64    `json:"usedMiB"`
	CapacityMiB int64    `json:"capacityMiB"`
	Enforced    bool     `json:"enforced"` // the size limit is enforced by the filesystem
	Message     string   `json:"message,omitempty"`
}

type Volume struct {
	Kind     string       `json:"kind"`
	Metadata Meta         `json:"metadata"`
	Spec     VolumeSpec   `json:"spec"`
	Status   VolumeStatus `json:"status"`
}

// DockerName is the name of the Docker volume (or mount directory) backing a cluster Volume.
func (v *Volume) DockerName() string {
	return "nube_" + v.Metadata.Namespace + "_" + v.Metadata.Name
}

// ---------------------------------------------------------------- Jobs

type JobSpec struct {
	Completions             int               `json:"completions"`                       // default 1
	Parallelism             int               `json:"parallelism"`                       // default 1
	BackoffLimit            *int              `json:"backoffLimit,omitempty"`            // default 6 failed pods
	ActiveDeadlineSeconds   int               `json:"activeDeadlineSeconds,omitempty"`   // 0 = no deadline
	TTLSecondsAfterFinished *int              `json:"ttlSecondsAfterFinished,omitempty"` // delete the job this long after it finishes
	NodeSelector            map[string]string `json:"nodeSelector,omitempty"`
	Template                PodTemplate       `json:"template"`
}

type JobStatus struct {
	Phase          string    `json:"phase"` // Running | Complete | Failed
	Active         int       `json:"active"`
	Succeeded      int       `json:"succeeded"`
	Failed         int       `json:"failed"`
	StartTime      time.Time `json:"startTime,omitempty"`
	CompletionTime time.Time `json:"completionTime,omitempty"`
	Message        string    `json:"message,omitempty"`
}

type Job struct {
	Kind     string    `json:"kind"`
	Metadata Meta      `json:"metadata"`
	Spec     JobSpec   `json:"spec"`
	Status   JobStatus `json:"status"`
}

type CronJobSpec struct {
	Schedule                   string  `json:"schedule"`                    // "*/5 * * * *", "@hourly" ...
	TimeZone                   string  `json:"timeZone,omitempty"`          // IANA name, default server local time
	ConcurrencyPolicy          string  `json:"concurrencyPolicy,omitempty"` // Allow | Forbid | Replace
	Suspend                    bool    `json:"suspend,omitempty"`
	StartingDeadlineSeconds    int     `json:"startingDeadlineSeconds,omitempty"`
	SuccessfulJobsHistoryLimit *int    `json:"successfulJobsHistoryLimit,omitempty"` // default 3
	FailedJobsHistoryLimit     *int    `json:"failedJobsHistoryLimit,omitempty"`     // default 1
	JobTemplate                JobSpec `json:"jobTemplate"`
}

type CronJobStatus struct {
	LastScheduleTime   time.Time `json:"lastScheduleTime,omitempty"`
	LastSuccessfulTime time.Time `json:"lastSuccessfulTime,omitempty"`
	NextScheduleTime   time.Time `json:"nextScheduleTime,omitempty"`
	Active             []string  `json:"active,omitempty"`
	Message            string    `json:"message,omitempty"`
}

type CronJob struct {
	Kind     string        `json:"kind"`
	Metadata Meta          `json:"metadata"`
	Spec     CronJobSpec   `json:"spec"`
	Status   CronJobStatus `json:"status"`
}

// ---------------------------------------------------------------- StatefulSets

// VolumeClaimTemplate creates one Volume per StatefulSet replica,
// named <name>-<statefulset>-<ordinal>.
type VolumeClaimTemplate struct {
	Name string     `json:"name"`
	Spec VolumeSpec `json:"spec"`
}

type StatefulSetSpec struct {
	Replicas             int                   `json:"replicas"`
	ServiceName          string                `json:"serviceName,omitempty"`         // headless service for <pod>.<service> DNS
	PodManagementPolicy  string                `json:"podManagementPolicy,omitempty"` // OrderedReady (default) | Parallel
	NodeSelector         map[string]string     `json:"nodeSelector,omitempty"`
	Template             PodTemplate           `json:"template"`
	VolumeClaimTemplates []VolumeClaimTemplate `json:"volumeClaimTemplates,omitempty"`
}

type StatefulSetStatus struct {
	Replicas        int    `json:"replicas"`
	ReadyReplicas   int    `json:"readyReplicas"`
	UpdatedReplicas int    `json:"updatedReplicas"`
	CurrentHash     string `json:"currentHash"`
	Revision        int    `json:"revision"`
	Phase           string `json:"phase"` // Healthy | Progressing | Degraded
	Message         string `json:"message,omitempty"`
}

type StatefulSet struct {
	Kind     string            `json:"kind"`
	Metadata Meta              `json:"metadata"`
	Spec     StatefulSetSpec   `json:"spec"`
	Status   StatefulSetStatus `json:"status"`
}

// ---------------------------------------------------------------- Ingress

type IngressBackend struct {
	Service string `json:"service"`
	Port    int    `json:"port"` // service port
}

type IngressPath struct {
	Path     string         `json:"path"`               // default "/"
	PathType string         `json:"pathType,omitempty"` // Prefix (default) | Exact
	Backend  IngressBackend `json:"backend"`
}

type IngressRule struct {
	Host  string        `json:"host,omitempty"` // "app.example.com", "*.example.com" or "" (any)
	Paths []IngressPath `json:"paths"`
}

type IngressTLS struct {
	Hosts      []string `json:"hosts"`
	SecretName string   `json:"secretName,omitempty"` // Secret of type tls; empty = self-signed
}

type IngressSpec struct {
	Rules        []IngressRule `json:"rules"`
	TLS          []IngressTLS  `json:"tls,omitempty"`
	RedirectHTTP bool          `json:"redirectHTTP,omitempty"` // send http:// to https:// for TLS hosts
}

type IngressStatus struct {
	Message  string `json:"message,omitempty"`
	Requests int64  `json:"requests"`
}

type Ingress struct {
	Kind     string        `json:"kind"`
	Metadata Meta          `json:"metadata"`
	Spec     IngressSpec   `json:"spec"`
	Status   IngressStatus `json:"status"`
}

// ---------------------------------------------------------------- Access control

const (
	RoleAdmin = "admin" // everything
	RoleEdit  = "edit"  // read + change workloads/config in its namespaces
	RoleView  = "view"  // read-only (no secret values)
	RoleNode  = "node"  // agent: register nodes and sync pods only
)

// Token is an API credential. The secret is shown once at creation; only its
// hash is stored.
type Token struct {
	ID         string    `json:"id"`
	Name       string    `json:"name"`
	Role       string    `json:"role"`
	Namespaces []string  `json:"namespaces,omitempty"` // empty = all namespaces
	Hash       string    `json:"hash,omitempty"`
	CreatedAt  time.Time `json:"createdAt"`
	Secret     string    `json:"secret,omitempty"` // only in the create response
}

// Identity is who a request was authenticated as.
type Identity struct {
	Name       string   `json:"name"`
	Role       string   `json:"role"`
	Namespaces []string `json:"namespaces,omitempty"`
}

// ---------------------------------------------------------------- Control plane

// ServerMember is one control-plane server in an HA cluster.
type ServerMember struct {
	ID       string    `json:"id"`
	RaftAddr string    `json:"raftAddr"`
	APIAddr  string    `json:"apiAddr"` // URL reachable by other servers
	Leader   bool      `json:"leader"`
	Voter    bool      `json:"voter"`
	JoinedAt time.Time `json:"joinedAt"`
}

type ClusterInfo struct {
	Mode     string         `json:"mode"` // single | ha
	ServerID string         `json:"serverID"`
	Leader   string         `json:"leader"`
	IsLeader bool           `json:"isLeader"`
	Term     uint64         `json:"term"`
	Members  []ServerMember `json:"members"`
	State    string         `json:"state"`
	TLS      bool           `json:"tls"`
	CAHash   string         `json:"caHash,omitempty"` // sha256 of the cluster CA, for pinning
}

// ---------------------------------------------------------------- Events

type Event struct {
	ID        int64     `json:"id"`
	Time      time.Time `json:"time"`
	Type      string    `json:"type"` // Normal | Warning
	Kind      string    `json:"kind"`
	Namespace string    `json:"namespace,omitempty"`
	Name      string    `json:"name"`
	Reason    string    `json:"reason"`
	Message   string    `json:"message"`
}

// ---------------------------------------------------------------- Agent protocol

// PodReport is what an agent tells the server about one pod it runs.
type PodReport struct {
	Namespace    string            `json:"namespace"`
	Name         string            `json:"name"`
	UID          string            `json:"uid"`
	Phase        PodPhase          `json:"phase"`
	Ready        bool              `json:"ready"`
	Message      string            `json:"message,omitempty"`
	ContainerID  string            `json:"containerID,omitempty"`
	Containers   []ContainerStatus `json:"containers,omitempty"`
	PodIP        string            `json:"podIP,omitempty"`
	HostPorts    map[int]int       `json:"hostPorts,omitempty"`
	HostPortsUDP map[int]int       `json:"hostPortsUDP,omitempty"`
	Restarts     int               `json:"restarts"`
	ExitCode     int               `json:"exitCode"`
	CPU          int64             `json:"cpu"`
	Memory       int64             `json:"memory"`
	StartedAt    time.Time         `json:"startedAt,omitempty"`
	FinishedAt   time.Time         `json:"finishedAt,omitempty"`
}

// VolumeReport is a node's measurement of a local volume.
type VolumeReport struct {
	Namespace   string `json:"namespace"`
	Name        string `json:"name"`
	UsedMiB     int64  `json:"usedMiB"`
	CapacityMiB int64  `json:"capacityMiB"`
	Enforced    bool   `json:"enforced"`
	Message     string `json:"message,omitempty"`
}

type SyncRequest struct {
	Node    Node           `json:"node"`
	Pods    []PodReport    `json:"pods"`
	Volumes []VolumeReport `json:"volumes,omitempty"`
	CSR     string         `json:"csr,omitempty"` // PEM certificate request for the agent API
}

// AssignedPod is a pod the agent should run, with env and volumes already resolved.
type AssignedPod struct {
	Pod     Pod                          `json:"pod"`
	Env     map[string]map[string]string `json:"env"`               // container name -> environment
	Volumes map[string]Volume            `json:"volumes,omitempty"` // by volume name
	Blocked string                       `json:"blocked,omitempty"` // reason the pod must not start yet
}

// NetworkPeer is another node the agent must route pod traffic to.
type NetworkPeer struct {
	Node        string `json:"node"`
	HostIP      string `json:"hostIP"`
	PodCIDR     string `json:"podCIDR"`
	WGPublicKey string `json:"wgPublicKey,omitempty"`
	WGPort      int    `json:"wgPort,omitempty"`
}

type NetworkConfig struct {
	ClusterCIDR string        `json:"clusterCIDR"`
	PodCIDR     string        `json:"podCIDR"` // this node's subnet
	Domain      string        `json:"domain"`  // e.g. "nube"
	Peers       []NetworkPeer `json:"peers"`
}

// PodEndpoint is one ready backend of a service.
type PodEndpoint struct {
	Pod          string      `json:"pod"`
	Node         string      `json:"node"`
	PodIP        string      `json:"podIP,omitempty"`
	HostIP       string      `json:"hostIP,omitempty"`
	HostPorts    map[int]int `json:"hostPorts,omitempty"`
	HostPortsUDP map[int]int `json:"hostPortsUDP,omitempty"`
}

// ServiceRoute tells agents how to serve a service inside the cluster
// (node-local proxy on the bridge gateway + DNS).
type ServiceRoute struct {
	Namespace string        `json:"namespace"`
	Name      string        `json:"name"`
	Ports     []ServicePort `json:"ports"`
	Headless  bool          `json:"headless,omitempty"`
	Endpoints []PodEndpoint `json:"endpoints"`
}

type SyncResponse struct {
	Pods        []AssignedPod     `json:"pods"`
	Network     NetworkConfig     `json:"network"`
	Services    []ServiceRoute    `json:"services"`
	PodIPs      map[string]string `json:"podIPs"`                // "pod.namespace" -> IP
	Records     map[string]string `json:"records,omitempty"`     // extra DNS names (without domain), e.g. "db-0.db.default.svc"
	Certificate string            `json:"certificate,omitempty"` // signed agent cert (PEM), answer to SyncRequest.CSR
	CA          string            `json:"ca,omitempty"`          // cluster CA (PEM)
}

// Overview is the single payload the dashboard polls.
type Overview struct {
	Version      string         `json:"version"`
	StartedAt    time.Time      `json:"startedAt"`
	Nodes        []*Node        `json:"nodes"`
	Deployments  []*Deployment  `json:"deployments"`
	StatefulSets []*StatefulSet `json:"statefulSets"`
	Jobs         []*Job         `json:"jobs"`
	CronJobs     []*CronJob     `json:"cronJobs"`
	Pods         []*Pod         `json:"pods"`
	Services     []*Service     `json:"services"`
	Ingresses    []*Ingress     `json:"ingresses"`
	ConfigMaps   []*ConfigMap   `json:"configMaps"`
	Secrets      []*Secret      `json:"secrets"`
	Volumes      []*Volume      `json:"volumes"`
	Events       []Event        `json:"events"`
	Namespaces   []string       `json:"namespaces"`
	Cluster      ClusterInfo    `json:"cluster"`
	Me           Identity       `json:"me"`
}

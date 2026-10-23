package agent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// Docker is a minimal Docker Engine API client (no external dependencies).
type Docker struct {
	base   string
	client *http.Client // short requests
	stream *http.Client // pulls, logs (no timeout)
}

// NewDocker connects to DOCKER_HOST or the default unix socket.
func NewDocker(host string) (*Docker, error) {
	if host == "" {
		host = os.Getenv("DOCKER_HOST")
	}
	if host == "" {
		host = "unix:///var/run/docker.sock"
	}
	u, err := url.Parse(host)
	if err != nil {
		return nil, err
	}
	var dial func(ctx context.Context, _, _ string) (net.Conn, error)
	base := "http://docker"
	switch u.Scheme {
	case "unix":
		path := u.Path
		dial = func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", path)
		}
	case "tcp", "http":
		base = "http://" + u.Host
	default:
		return nil, fmt.Errorf("unsupported docker host %q", host)
	}
	mk := func(timeout time.Duration) *http.Client {
		tr := &http.Transport{MaxIdleConns: 10, IdleConnTimeout: 30 * time.Second}
		if dial != nil {
			tr.DialContext = dial
		}
		return &http.Client{Transport: tr, Timeout: timeout}
	}
	return &Docker{base: base, client: mk(60 * time.Second), stream: mk(0)}, nil
}

type dockerError struct {
	Code    int
	Message string
}

func (e *dockerError) Error() string { return fmt.Sprintf("docker: %s (%d)", e.Message, e.Code) }

func isNotFound(err error) bool {
	var de *dockerError
	return errors.As(err, &de) && de.Code == 404
}

func (d *Docker) do(ctx context.Context, c *http.Client, method, path string, body any, out any) (*http.Response, error) {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, d.base+path, rd)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		defer resp.Body.Close()
		var e struct {
			Message string `json:"message"`
		}
		b, _ := io.ReadAll(resp.Body)
		json.Unmarshal(b, &e)
		if e.Message == "" {
			e.Message = strings.TrimSpace(string(b))
		}
		return nil, &dockerError{Code: resp.StatusCode, Message: e.Message}
	}
	if out != nil {
		defer resp.Body.Close()
		return resp, json.NewDecoder(resp.Body).Decode(out)
	}
	return resp, nil
}

func (d *Docker) call(ctx context.Context, method, path string, body, out any) error {
	resp, err := d.do(ctx, d.client, method, path, body, out)
	if err == nil && out == nil {
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
	return err
}

// ---------------------------------------------------------------- info

type RuntimeInfo struct {
	Version string
	OS      string
	Kernel  string
	Arch    string
	NCPU    int
	MemMiB  int64
}

func (d *Docker) Info(ctx context.Context) (RuntimeInfo, error) {
	var v struct {
		ServerVersion   string
		OperatingSystem string
		KernelVersion   string
		Architecture    string
		NCPU            int
		MemTotal        int64
	}
	if err := d.call(ctx, "GET", "/info", nil, &v); err != nil {
		return RuntimeInfo{}, err
	}
	return RuntimeInfo{v.ServerVersion, v.OperatingSystem, v.KernelVersion, v.Architecture, v.NCPU, v.MemTotal >> 20}, nil
}

// ---------------------------------------------------------------- images

func splitImage(ref string) (name, tag string) {
	if i := strings.Index(ref, "@"); i >= 0 {
		return ref, ""
	}
	slash := strings.LastIndex(ref, "/")
	if c := strings.LastIndex(ref, ":"); c > slash {
		return ref[:c], ref[c+1:]
	}
	return ref, "latest"
}

func (d *Docker) imageExists(ctx context.Context, ref string) bool {
	return d.call(ctx, "GET", "/images/"+url.PathEscape(ref)+"/json", nil, nil) == nil
}

// EnsureImage pulls an image according to the pull policy.
func (d *Docker) EnsureImage(ctx context.Context, ref, policy string) error {
	_, tag := splitImage(ref)
	if policy == "" {
		policy = "IfNotPresent"
		if tag == "latest" {
			policy = "Always"
		}
	}
	exists := d.imageExists(ctx, ref)
	switch policy {
	case "Never":
		if !exists {
			return fmt.Errorf("image %s not present and imagePullPolicy is Never", ref)
		}
		return nil
	case "IfNotPresent":
		if exists {
			return nil
		}
	}
	name, tag := splitImage(ref)
	q := url.Values{"fromImage": {name}}
	if tag != "" {
		q.Set("tag", tag)
	}
	resp, err := d.do(ctx, d.stream, "POST", "/images/create?"+q.Encode(), nil, nil)
	if err != nil {
		if exists {
			return nil // keep running the cached image if the registry is unreachable
		}
		return err
	}
	defer resp.Body.Close()
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64*1024), 1<<20)
	for sc.Scan() {
		var msg struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(sc.Bytes(), &msg) == nil && msg.Error != "" {
			if exists {
				return nil
			}
			return errors.New(msg.Error)
		}
	}
	return sc.Err()
}

// ---------------------------------------------------------------- containers

type Mount struct {
	Type     string // bind | volume
	Source   string
	Target   string
	ReadOnly bool
}

type ContainerSpec struct {
	Name      string
	Image     string
	Command   []string
	Args      []string
	Env       []string
	Labels    map[string]string
	Ports     []PortSpec
	CPUMilli  int64
	MemoryMiB int64
	Mounts    []Mount
	Network   string // a network name, or "container:<id>" to join another container's network
	DNS       []string
	DNSSearch []string
	Hostname  string
	Init      bool // run tini as PID 1 (reaps zombies, forwards signals)
}

type PortSpec struct {
	ContainerPort int
	HostPort      int
	Protocol      string
}

type ContainerState struct {
	ID         string
	Name       string
	Labels     map[string]string
	Status     string // created | running | exited | dead | paused | restarting
	Running    bool
	ExitCode   int
	OOMKilled  bool
	Error      string
	StartedAt  time.Time
	FinishedAt time.Time
	HostPorts  map[int]int // TCP
	UDPPorts   map[int]int
	IP         string
	Image      string
}

func (d *Docker) Create(ctx context.Context, s ContainerSpec) (string, error) {
	exposed := map[string]struct{}{}
	bindings := map[string][]map[string]string{}
	for _, p := range s.Ports {
		proto := strings.ToLower(p.Protocol)
		if proto == "" {
			proto = "tcp"
		}
		k := fmt.Sprintf("%d/%s", p.ContainerPort, proto)
		exposed[k] = struct{}{}
		hp := ""
		if p.HostPort > 0 {
			hp = strconv.Itoa(p.HostPort)
		}
		bindings[k] = []map[string]string{{"HostIp": "", "HostPort": hp}}
	}
	var mounts []map[string]any
	for _, m := range s.Mounts {
		mounts = append(mounts, map[string]any{"Type": m.Type, "Source": m.Source, "Target": m.Target, "ReadOnly": m.ReadOnly})
	}
	host := map[string]any{
		"Mounts":        mounts,
		"RestartPolicy": map[string]any{"Name": "no"},
		"LogConfig":     map[string]any{"Type": "json-file", "Config": map[string]string{"max-size": "10m", "max-file": "3"}},
		"Init":          s.Init,
	}
	joined := strings.HasPrefix(s.Network, "container:")
	if !joined {
		host["PortBindings"] = bindings
	}
	if s.MemoryMiB > 0 {
		host["Memory"] = s.MemoryMiB << 20
	}
	if s.CPUMilli > 0 {
		host["NanoCpus"] = s.CPUMilli * 1_000_000
	}
	if s.Network != "" {
		host["NetworkMode"] = s.Network
	}
	if len(s.DNS) > 0 && !joined {
		host["Dns"] = s.DNS
		host["DnsSearch"] = s.DNSSearch
		// Try search domains first for "web" and "web.prod" (Docker defaults to ndots:0).
		host["DnsOptions"] = []string{"ndots:2"}
	}
	body := map[string]any{
		"Image":      s.Image,
		"Env":        s.Env,
		"Labels":     s.Labels,
		"HostConfig": host,
	}
	if !joined { // these belong to the network owner (the pod's pause container)
		body["ExposedPorts"] = exposed
		body["Hostname"] = s.Hostname
	}
	if len(s.Command) > 0 {
		body["Entrypoint"] = s.Command
	}
	if len(s.Args) > 0 {
		body["Cmd"] = s.Args
	}
	var out struct{ Id string }
	err := d.call(ctx, "POST", "/containers/create?name="+url.QueryEscape(s.Name), body, &out)
	return out.Id, err
}

func (d *Docker) Start(ctx context.Context, id string) error {
	err := d.call(ctx, "POST", "/containers/"+id+"/start", nil, nil)
	var de *dockerError
	if errors.As(err, &de) && de.Code == 304 {
		return nil
	}
	return err
}

func (d *Docker) Restart(ctx context.Context, id string) error {
	return d.call(ctx, "POST", "/containers/"+id+"/restart?t=5", nil, nil)
}

func (d *Docker) Stop(ctx context.Context, id string, grace time.Duration) error {
	err := d.call(ctx, "POST", fmt.Sprintf("/containers/%s/stop?t=%d", id, int(grace.Seconds())), nil, nil)
	var de *dockerError
	if errors.As(err, &de) && (de.Code == 304 || de.Code == 404) {
		return nil
	}
	return err
}

func (d *Docker) Remove(ctx context.Context, id string) error {
	err := d.call(ctx, "DELETE", "/containers/"+id+"?force=true&v=true", nil, nil)
	if isNotFound(err) {
		return nil
	}
	return err
}

// List returns all containers carrying the label nube.managed=true.
func (d *Docker) List(ctx context.Context) ([]ContainerState, error) {
	filters, _ := json.Marshal(map[string][]string{"label": {"nube.managed=true"}})
	var raw []struct {
		Id     string
		Names  []string
		Labels map[string]string
		State  string
	}
	if err := d.call(ctx, "GET", "/containers/json?all=true&filters="+url.QueryEscape(string(filters)), nil, &raw); err != nil {
		return nil, err
	}
	out := make([]ContainerState, 0, len(raw))
	for _, c := range raw {
		name := ""
		if len(c.Names) > 0 {
			name = strings.TrimPrefix(c.Names[0], "/")
		}
		out = append(out, ContainerState{ID: c.Id, Name: name, Labels: c.Labels, Status: c.State, Running: c.State == "running"})
	}
	return out, nil
}

func (d *Docker) Inspect(ctx context.Context, id string) (ContainerState, error) {
	var v struct {
		Id     string
		Name   string
		Config struct {
			Labels map[string]string
			Image  string
		}
		State struct {
			Status     string
			Running    bool
			ExitCode   int
			OOMKilled  bool
			Error      string
			StartedAt  time.Time
			FinishedAt time.Time
		}
		NetworkSettings struct {
			Ports    map[string][]struct{ HostIp, HostPort string }
			Networks map[string]struct{ IPAddress string }
		}
	}
	if err := d.call(ctx, "GET", "/containers/"+id+"/json", nil, &v); err != nil {
		return ContainerState{}, err
	}
	cs := ContainerState{
		ID: v.Id, Name: strings.TrimPrefix(v.Name, "/"), Labels: v.Config.Labels,
		Status: v.State.Status, Running: v.State.Running, ExitCode: v.State.ExitCode, OOMKilled: v.State.OOMKilled,
		Error: v.State.Error, StartedAt: v.State.StartedAt, FinishedAt: v.State.FinishedAt,
		HostPorts: map[int]int{}, UDPPorts: map[int]int{}, Image: v.Config.Image,
	}
	for k, binds := range v.NetworkSettings.Ports {
		port, proto, _ := strings.Cut(k, "/")
		cp, _ := strconv.Atoi(port)
		for _, b := range binds {
			if hp, _ := strconv.Atoi(b.HostPort); hp > 0 {
				if proto == "udp" {
					cs.UDPPorts[cp] = hp
				} else {
					cs.HostPorts[cp] = hp
				}
				break
			}
		}
	}
	if n, ok := v.NetworkSettings.Networks[networkName]; ok {
		cs.IP = n.IPAddress
	}
	if cs.IP == "" {
		for _, n := range v.NetworkSettings.Networks {
			if n.IPAddress != "" {
				cs.IP = n.IPAddress
				break
			}
		}
	}
	return cs, nil
}

// Logs writes demultiplexed stdout/stderr of a container to w.
func (d *Docker) Logs(ctx context.Context, id, tail string, follow bool, w io.Writer) error {
	if tail == "" {
		tail = "500"
	}
	q := url.Values{"stdout": {"1"}, "stderr": {"1"}, "tail": {tail}, "timestamps": {"0"}}
	if follow {
		q.Set("follow", "1")
	}
	resp, err := d.do(ctx, d.stream, "GET", "/containers/"+id+"/logs?"+q.Encode(), nil, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	flush := func() {
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}
	// Containers without a TTY use the multiplexed stream format:
	// [stream(1) 0 0 0 size(4 big-endian)] payload
	hdr := make([]byte, 8)
	br := bufio.NewReader(resp.Body)
	for {
		if _, err := io.ReadFull(br, hdr); err != nil {
			if err == io.EOF || err == io.ErrUnexpectedEOF || ctx.Err() != nil {
				return nil
			}
			return err
		}
		if hdr[0] > 2 || hdr[1] != 0 {
			// Not multiplexed (TTY): copy raw.
			w.Write(hdr)
			_, err := io.Copy(w, br)
			return err
		}
		n := binary.BigEndian.Uint32(hdr[4:])
		if _, err := io.CopyN(w, br, int64(n)); err != nil {
			return nil
		}
		flush()
	}
}

// Stats returns cumulative CPU time (ns) and working-set memory (bytes).
func (d *Docker) Stats(ctx context.Context, id string) (cpuNS uint64, memBytes uint64, err error) {
	var v struct {
		CPUStats struct {
			CPUUsage struct {
				TotalUsage uint64 `json:"total_usage"`
			} `json:"cpu_usage"`
		} `json:"cpu_stats"`
		MemoryStats struct {
			Usage uint64            `json:"usage"`
			Stats map[string]uint64 `json:"stats"`
		} `json:"memory_stats"`
	}
	if err := d.call(ctx, "GET", "/containers/"+id+"/stats?stream=false&one-shot=true", nil, &v); err != nil {
		return 0, 0, err
	}
	mem := v.MemoryStats.Usage
	inactive := v.MemoryStats.Stats["inactive_file"]
	if inactive == 0 {
		inactive = v.MemoryStats.Stats["total_inactive_file"]
	}
	if inactive < mem {
		mem -= inactive
	}
	return v.CPUStats.CPUUsage.TotalUsage, mem, nil
}

// ---------------------------------------------------------------- networks & volumes

type NetworkInfo struct {
	ID      string
	Subnet  string
	Gateway string
	Count   int
}

func (d *Docker) InspectNetwork(ctx context.Context, name string) (*NetworkInfo, error) {
	var v struct {
		Id   string
		IPAM struct {
			Config []struct{ Subnet, Gateway string }
		}
		Containers map[string]any
	}
	err := d.call(ctx, "GET", "/networks/"+url.PathEscape(name), nil, &v)
	if isNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	ni := &NetworkInfo{ID: v.Id, Count: len(v.Containers)}
	if len(v.IPAM.Config) > 0 {
		ni.Subnet, ni.Gateway = v.IPAM.Config[0].Subnet, v.IPAM.Config[0].Gateway
	}
	return ni, nil
}

func (d *Docker) CreateNetwork(ctx context.Context, name, subnet, gateway string, opts map[string]string) error {
	body := map[string]any{
		"Name":           name,
		"Driver":         "bridge",
		"CheckDuplicate": true,
		"IPAM":           map[string]any{"Config": []map[string]string{{"Subnet": subnet, "Gateway": gateway}}},
		"Options":        opts,
		"Labels":         map[string]string{"nube.managed": "true"},
	}
	return d.call(ctx, "POST", "/networks/create", body, nil)
}

func (d *Docker) RemoveNetwork(ctx context.Context, name string) error {
	err := d.call(ctx, "DELETE", "/networks/"+url.PathEscape(name), nil, nil)
	if isNotFound(err) {
		return nil
	}
	return err
}

func (d *Docker) EnsureVolume(ctx context.Context, name string, driverOpts, labels map[string]string) error {
	if d.call(ctx, "GET", "/volumes/"+url.PathEscape(name), nil, nil) == nil {
		return nil
	}
	l := map[string]string{"nube.managed": "true"}
	for k, v := range labels {
		l[k] = v
	}
	body := map[string]any{"Name": name, "Driver": "local", "DriverOpts": driverOpts, "Labels": l}
	return d.call(ctx, "POST", "/volumes/create", body, nil)
}

// ListVolumes returns the names of volumes carrying a label ("key=value").
func (d *Docker) ListVolumes(ctx context.Context, label string) ([]string, error) {
	filters, _ := json.Marshal(map[string][]string{"label": {label}})
	var out struct {
		Volumes []struct{ Name string }
	}
	if err := d.call(ctx, "GET", "/volumes?filters="+url.QueryEscape(string(filters)), nil, &out); err != nil {
		return nil, err
	}
	names := make([]string, 0, len(out.Volumes))
	for _, v := range out.Volumes {
		names = append(names, v.Name)
	}
	return names, nil
}

// ImportImage creates an image from a root filesystem tarball (docker import).
func (d *Docker) ImportImage(ctx context.Context, ref string, changes []string, tarball io.Reader) error {
	name, tag := splitImage(ref)
	q := url.Values{"fromSrc": {"-"}, "repo": {name}, "tag": {tag}}
	for _, c := range changes {
		q.Add("changes", c)
	}
	req, err := http.NewRequestWithContext(ctx, "POST", d.base+"/images/create?"+q.Encode(), tarball)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-tar")
	resp, err := d.stream.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 || strings.Contains(string(body), `"error"`) {
		return fmt.Errorf("docker import: %s", strings.TrimSpace(string(body)))
	}
	return nil
}

// ImageExists reports whether an image is present locally.
func (d *Docker) ImageExists(ctx context.Context, ref string) bool { return d.imageExists(ctx, ref) }

func (d *Docker) RemoveVolume(ctx context.Context, name string) error {
	err := d.call(ctx, "DELETE", "/volumes/"+url.PathEscape(name)+"?force=true", nil, nil)
	if isNotFound(err) {
		return nil
	}
	return err
}

// VolumeExists reports whether a Docker volume exists.
func (d *Docker) VolumeExists(ctx context.Context, name string) bool {
	return d.call(ctx, "GET", "/volumes/"+url.PathEscape(name), nil, nil) == nil
}

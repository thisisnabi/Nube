package server

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/thisisnabi/Nube/internal/api"
	"github.com/thisisnabi/Nube/internal/store"
)

// Finding is one problem found by diagnose: what is wrong, the evidence,
// and what to do about it. Written for people and for AI agents alike.
type Finding struct {
	Severity  string   `json:"severity"` // critical | warning | info
	Object    string   `json:"object"`   // kind/name
	Namespace string   `json:"namespace,omitempty"`
	Owner     string   `json:"owner,omitempty"` // kind/name of the workload a pod belongs to
	Problem   string   `json:"problem"`
	Evidence  []string `json:"evidence,omitempty"`
	Fix       string   `json:"fix"`
	Command   string   `json:"command,omitempty"` // a command that helps (inspect or fix)
	Logs      string   `json:"logs,omitempty"`    // last lines of a failing container
}

type diagnosis struct {
	Healthy  bool      `json:"healthy"`
	Summary  string    `json:"summary"`
	Findings []Finding `json:"findings"`
}

var severityRank = map[string]int{"critical": 0, "warning": 1, "info": 2}

// diagnose inspects the cluster (or one namespace / object) and explains
// what is broken and how to fix it.
func (s *Server) diagnose(st *store.State, ns, kind, name string) []Finding {
	var out []Finding
	add := func(f Finding) { out = append(out, f) }
	inNS := func(n string) bool { return ns == "" || n == ns }
	owner := func(p *api.Pod) string {
		if p.Spec.Owner == "" {
			return ""
		}
		return strings.ToLower(p.Spec.OwnerKind) + "/" + p.Spec.Owner
	}

	if ns == "" && kind == "" {
		for _, n := range st.Nodes {
			n = s.live.node(n)
			switch {
			case n.Status != api.NodeReady:
				add(Finding{Severity: "critical", Object: "node/" + n.Name, Problem: "node is " + string(n.Status),
					Evidence: []string{"last heartbeat " + time.Since(n.LastHeartbeat).Round(time.Second).String() + " ago"},
					Fix:      "check the agent on the machine (journalctl -u nube-agent) and that it reaches port 7070; pods on it are rescheduled",
					Command:  "nube describe node " + n.Name})
			case n.NetworkMode == "wireguard" && !n.NetworkReady:
				add(Finding{Severity: "warning", Object: "node/" + n.Name, Problem: "pod network (WireGuard) is down",
					Evidence: []string{n.NetworkError}, Fix: "install wireguard-tools and open UDP 51820 between nodes"})
			}
		}
	}

	for _, p := range st.Pods {
		if !inNS(p.Metadata.Namespace) || p.Status.Phase == api.PodTerminating {
			continue
		}
		obj := "pod/" + p.Metadata.Name
		f := Finding{Object: obj, Namespace: p.Metadata.Namespace, Owner: owner(p)}
		msg := p.Status.Message
		var bad *api.ContainerStatus
		for i := range p.Status.Containers {
			c := &p.Status.Containers[i]
			if c.Message != "" || c.Restarts > 0 && !c.Ready {
				bad = c
				break
			}
		}
		if bad != nil {
			f.Evidence = append(f.Evidence, fmt.Sprintf("container %s: %s, restarts %d, exit code %d %s", bad.Name, bad.State, bad.Restarts, bad.ExitCode, bad.Message))
		}
		logsCmd := "nube logs " + p.Metadata.Name
		if bad != nil {
			logsCmd += " -c " + bad.Name
		}
		switch {
		case strings.Contains(msg, "OOMKilled") || bad != nil && strings.Contains(bad.Message, "OOMKilled"):
			f.Severity, f.Problem = "critical", "container is killed for running out of memory"
			f.Fix = "raise resources.memory (it is also the limit), or fix the leak; `nube top workloads` suggests a value"
			f.Command = "nube top workloads"
		case strings.HasPrefix(msg, "ErrImagePull") || bad != nil && strings.HasPrefix(bad.Message, "ErrImagePull"):
			f.Severity, f.Problem = "critical", "image cannot be pulled"
			f.Evidence = append(f.Evidence, msg)
			f.Fix = "check the image name and tag, and that the node can reach the registry; then `nube set image`"
		case p.Status.Phase == api.PodCrashLoop || p.Status.Phase == api.PodFailed:
			f.Severity, f.Problem = "critical", "container keeps exiting ("+string(p.Status.Phase)+")"
			f.Fix = "read the logs below for the cause (bad config, missing env, failing start command)"
			f.Command = logsCmd
			if bad != nil {
				f.Logs = s.tailLogs(st, p, bad.ContainerID, 20)
			}
		case strings.Contains(msg, "health check failed") || bad != nil && strings.Contains(bad.Message, "health check failed"):
			f.Severity, f.Problem = "warning", "health check failing"
			f.Fix = "check that healthCheck's port/path match what the app serves, and give it time to start"
			f.Command = logsCmd
		case strings.HasPrefix(msg, "waiting for configmap/") || strings.HasPrefix(msg, "waiting for secret/"):
			f.Severity, f.Problem = "critical", "references configuration that does not exist"
			f.Evidence = append(f.Evidence, msg)
			f.Fix = "create the ConfigMap/Secret named in the message (or fix envFrom)"
		case strings.HasPrefix(msg, "waiting for service"):
			f.Severity, f.Problem = "info", "waiting for a dependency (dependsOn)"
			f.Evidence = append(f.Evidence, msg)
			f.Fix = "make the service's pods ready; this pod starts on its own afterwards"
		case p.Spec.NodeName == "" && msg != "":
			f.Severity, f.Problem = "critical", "pod cannot be scheduled"
			f.Evidence = append(f.Evidence, msg)
			f.Fix = "lower resources requests, relax nodeSelector, free a host port, or add a node"
			f.Command = "nube get nodes -o wide"
		default:
			continue
		}
		add(f)
	}

	for _, d := range st.Deployments {
		if !inNS(d.Metadata.Namespace) {
			continue
		}
		if d.Status.LastRollback != "" && d.Status.Phase == "Healthy" {
			add(Finding{Severity: "warning", Object: "deployment/" + d.Metadata.Name, Namespace: d.Metadata.Namespace,
				Problem: "the last rollout was rolled back automatically", Evidence: []string{d.Status.LastRollback},
				Fix: "fix the new version before applying it again", Command: "nube rollout history deploy " + d.Metadata.Name})
		}
		if r := s.live.recommend(api.KindDeployment, d.Metadata.Namespace, d.Metadata.Name, &d.Spec.Template); r != nil && strings.Contains(r.Message, "OOM") {
			add(Finding{Severity: "warning", Object: "deployment/" + d.Metadata.Name, Namespace: d.Metadata.Namespace,
				Problem: "memory use is close to the limit", Evidence: []string{r.Message},
				Fix: fmt.Sprintf("set resources.memory to %dMi", r.Memory)})
		}
	}

	for _, svc := range st.Services {
		if !inNS(svc.Metadata.Namespace) || len(svc.Spec.Selector) == 0 {
			continue
		}
		matching, ready := 0, 0
		labelSets := map[string]bool{}
		for _, p := range st.Pods {
			if p.Metadata.Namespace != svc.Metadata.Namespace {
				continue
			}
			if labelsMatch(p.Metadata.Labels, svc.Spec.Selector) {
				matching++
				if p.Status.Ready {
					ready++
				}
			} else if app := p.Metadata.Labels["app"]; app != "" {
				labelSets["app="+app] = true
			}
		}
		f := Finding{Object: "service/" + svc.Metadata.Name, Namespace: svc.Metadata.Namespace}
		switch {
		case matching == 0:
			f.Severity, f.Problem = "critical", "selector matches no pods: the service has nowhere to send traffic"
			f.Evidence = []string{"selector " + labelString(svc.Spec.Selector)}
			if len(labelSets) > 0 {
				f.Evidence = append(f.Evidence, "pods in the namespace have "+strings.Join(sortedSet(labelSets), ", "))
			}
			f.Fix = "make spec.selector match the pods' template labels"
		case ready == 0:
			f.Severity, f.Problem = "critical", fmt.Sprintf("none of its %d pods is ready", matching)
			f.Fix = "fix the pods (see the pod findings)"
		default:
			continue
		}
		add(f)
	}

	for _, ing := range st.Ingresses {
		if !inNS(ing.Metadata.Namespace) {
			continue
		}
		for _, rule := range ing.Spec.Rules {
			for _, path := range rule.Paths {
				if st.Services[ing.Metadata.Namespace+"/"+path.Backend.Service] == nil {
					add(Finding{Severity: "critical", Object: "ingress/" + ing.Metadata.Name, Namespace: ing.Metadata.Namespace,
						Problem:  "routes to a service that does not exist",
						Evidence: []string{rule.Host + path.Path + " -> " + path.Backend.Service},
						Fix:      "create service " + path.Backend.Service + " or fix the backend"})
				}
			}
		}
		if m := s.ingress.acmeStatus(ing); strings.Contains(m, "acme") && strings.Contains(m, ":") {
			add(Finding{Severity: "warning", Object: "ingress/" + ing.Metadata.Name, Namespace: ing.Metadata.Namespace,
				Problem: "certificate could not be obtained", Evidence: []string{m},
				Fix: "DNS must point at the servers and port 80 or 443 must be reachable from the internet"})
		}
	}

	for _, v := range st.Volumes {
		if !inNS(v.Metadata.Namespace) {
			continue
		}
		v = s.live.volume(v)
		if v.Status.CapacityMiB > 0 && v.Status.UsedMiB*100 >= v.Status.CapacityMiB*90 {
			add(Finding{Severity: "warning", Object: "volume/" + v.Metadata.Name, Namespace: v.Metadata.Namespace,
				Problem:  "volume is almost full",
				Evidence: []string{fmt.Sprintf("%d of %d MiB used", v.Status.UsedMiB, v.Status.CapacityMiB)},
				Fix:      "raise spec.size (grows online) or delete data"})
		}
		if v.Status.Message != "" && !v.Status.Enforced && v.Spec.Size != "" {
			add(Finding{Severity: "info", Object: "volume/" + v.Metadata.Name, Namespace: v.Metadata.Namespace,
				Problem: "volume note", Evidence: []string{v.Status.Message}, Fix: "see the message"})
		}
	}

	for _, j := range st.Jobs {
		if inNS(j.Metadata.Namespace) && j.Status.Phase == "Failed" {
			add(Finding{Severity: "warning", Object: "job/" + j.Metadata.Name, Namespace: j.Metadata.Namespace,
				Problem: "job failed", Evidence: []string{j.Status.Message}, Fix: "read the logs of its pods, fix, and re-run it",
				Command: "nube get pods -n " + j.Metadata.Namespace})
		}
	}

	if kind != "" && name != "" {
		target := strings.ToLower(kind) + "/" + name
		var keep []Finding
		for _, f := range out {
			if f.Object == target || f.Owner == target {
				keep = append(keep, f)
			}
		}
		out = keep
	}
	sort.SliceStable(out, func(i, j int) bool {
		if severityRank[out[i].Severity] != severityRank[out[j].Severity] {
			return severityRank[out[i].Severity] < severityRank[out[j].Severity]
		}
		return out[i].Namespace+out[i].Object < out[j].Namespace+out[j].Object
	})
	return out
}

func labelString(m map[string]string) string {
	var parts []string
	for k, v := range m {
		parts = append(parts, k+"="+v)
	}
	sort.Strings(parts)
	return strings.Join(parts, ",")
}

func sortedSet(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// tailLogs fetches the last lines of a container from its node (best effort).
func (s *Server) tailLogs(st *store.State, p *api.Pod, cid string, lines int) string {
	n := st.Nodes[p.Spec.NodeName]
	if n == nil || cid == "" {
		return ""
	}
	node := *n
	client, base, err := s.agentClient(&node)
	if err != nil {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	q := url.Values{"id": {cid}, "tail": {fmt.Sprint(lines)}}
	req, _ := http.NewRequestWithContext(ctx, "GET", base+"/logs?"+q.Encode(), nil)
	if !node.TLS {
		req.Header.Set("Authorization", "Bearer "+s.cfg.Token)
	}
	resp, err := client.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<10))
	return strings.TrimSpace(string(b))
}

func (s *Server) handleDiagnose(w http.ResponseWriter, r *http.Request) {
	if !s.store.IsLeader() && s.store.LeaderAPI() != "" && r.Header.Get("X-Nube-Forwarded") == "" {
		s.forwardToLeader(w, r) // live usage is on the leader
		return
	}
	id := identityFrom(r)
	q := r.URL.Query()
	ns, kind, name := q.Get("namespace"), q.Get("kind"), q.Get("name")
	if kind != "" {
		kind = resolveKindName(kind)
	}
	if ns != "" && !nsAllowed(id, ns) {
		writeErr(w, 403, id.Name+" may not access namespace "+ns)
		return
	}
	var d diagnosis
	s.store.Read(func(st *store.State) {
		for _, f := range s.diagnose(st, ns, kind, name) {
			if f.Namespace == "" && len(id.Namespaces) > 0 || f.Namespace != "" && !nsAllowed(id, f.Namespace) {
				continue
			}
			d.Findings = append(d.Findings, f)
		}
	})
	if d.Findings == nil {
		d.Findings = []Finding{}
	}
	counts := map[string]int{}
	for _, f := range d.Findings {
		counts[f.Severity]++
	}
	d.Healthy = counts["critical"] == 0 && counts["warning"] == 0
	if len(d.Findings) == 0 {
		d.Summary = "no problems found"
	} else {
		d.Summary = fmt.Sprintf("%d critical, %d warning, %d info", counts["critical"], counts["warning"], counts["info"])
	}
	writeJSON(w, 200, d)
}

// resolveKindName turns "deploy", "deployments" ... into a Kind.
func resolveKindName(k string) string {
	if n := api.NormalizeKind(k); n != "" {
		return n
	}
	return k
}

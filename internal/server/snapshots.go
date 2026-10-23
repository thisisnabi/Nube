package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/thisisnabi/Nube/internal/api"
	"github.com/thisisnabi/Nube/internal/store"
)

const (
	recoverAfter    = 2 * time.Minute // a node silent this long loses its local volumes (autoRecover)
	snapshotTimeout = 30 * time.Minute
)

func applyVolumeSnapshot(st *store.State, o api.Object) (string, error) {
	var vs api.VolumeSnapshot
	if err := json.Unmarshal(o.Raw, &vs); err != nil {
		return "", err
	}
	if vs.Spec.Volume == "" || st.Volumes[o.Namespace+"/"+vs.Spec.Volume] == nil {
		return "", fmt.Errorf("volumesnapshot/%s: spec.volume %q does not exist", o.Name, vs.Spec.Volume)
	}
	key := o.Namespace + "/" + o.Name
	if old := st.Snapshots[key]; old != nil {
		if old.Spec.Volume == vs.Spec.Volume {
			return "unchanged", nil
		}
		return "", fmt.Errorf("volumesnapshot/%s: snapshots are immutable", o.Name)
	}
	createSnapshot(st, o.Namespace, o.Name, vs.Spec.Volume, vs.Metadata.Labels)
	return "created", nil
}

func createSnapshot(st *store.State, ns, name, volume string, labels map[string]string) *api.VolumeSnapshot {
	now := time.Now()
	l := map[string]string{"nube/volume": volume}
	for k, v := range labels {
		l[k] = v
	}
	vs := &api.VolumeSnapshot{
		Kind:     api.KindVolumeSnapshot,
		Metadata: api.Meta{Name: name, Namespace: ns, UID: newUID(), Labels: l, CreatedAt: now, UpdatedAt: now},
		Spec:     api.VolumeSnapshotSpec{Volume: volume},
		Status:   api.VolumeSnapshotStatus{Phase: "Pending"},
	}
	st.Snapshots[vs.Metadata.Key()] = vs
	st.Event("Normal", api.KindVolumeSnapshot, ns, name, "Created", "snapshot of volume "+volume+" requested")
	return vs
}

// snapshotNode picks the node that can read a volume's data.
func snapshotNode(st *store.State, v *api.Volume) string {
	if v.Status.NodeName != "" {
		return v.Status.NodeName
	}
	if v.Spec.Type == "local" {
		return ""
	}
	for _, name := range sortedKeys(st.Nodes) {
		if st.Nodes[name].Status == api.NodeReady {
			return name
		}
	}
	return ""
}

func reconcileSnapshots(st *store.State, now time.Time) {
	for _, vs := range st.Snapshots {
		if vs.Status.Phase != "Pending" && vs.Status.Phase != "Uploading" {
			continue
		}
		v := st.Volumes[vs.Metadata.Namespace+"/"+vs.Spec.Volume]
		switch {
		case v == nil:
			vs.Status.Phase, vs.Status.Message = "Failed", "volume no longer exists"
		case now.Sub(vs.Metadata.CreatedAt) > snapshotTimeout:
			vs.Status.Phase, vs.Status.Message = "Failed", "timed out waiting for the node to upload the snapshot"
		case restorePending(v):
			vs.Status.Node, vs.Status.Message = "", "waiting for the volume to be restored first"
		case vs.Status.Node == "":
			if n := snapshotNode(st, v); n != "" {
				vs.Status.Node = n
			} else {
				vs.Status.Message = "volume has never been used on a node yet"
			}
		}
		if vs.Status.Phase == "Failed" {
			st.Event("Warning", api.KindVolumeSnapshot, vs.Metadata.Namespace, vs.Metadata.Name, "Failed", vs.Status.Message)
		}
	}
}

// reconcileBackups takes scheduled snapshots and prunes old ones.
func reconcileBackups(st *store.State, now time.Time) {
	for _, v := range st.Volumes {
		b := v.Spec.Backup
		if b == nil {
			continue
		}
		sched, err := parseSchedule(b.Schedule)
		if err != nil {
			continue
		}
		last := v.Status.LastBackup
		if last.IsZero() {
			last = v.Metadata.CreatedAt
		}
		next := sched.Next(last.In(time.Local))
		due := !next.IsZero() && !now.Before(next)
		hasData := v.Status.NodeName != "" || v.Spec.Type != "local"
		if due {
			name := fmt.Sprintf("%s-%s", v.Metadata.Name, now.UTC().Format("20060102-1504"))
			if hasData && !restorePending(v) && st.Snapshots[v.Metadata.Namespace+"/"+name] == nil {
				createSnapshot(st, v.Metadata.Namespace, name, v.Metadata.Name, map[string]string{"nube/backup": "true"})
			}
			v.Status.LastBackup = now // a volume never used has nothing to back up yet
		}
		keep := b.Keep
		if keep <= 0 {
			keep = 7
		}
		var backups []*api.VolumeSnapshot
		for _, s := range st.Snapshots {
			if s.Metadata.Namespace == v.Metadata.Namespace && s.Spec.Volume == v.Metadata.Name && s.Metadata.Labels["nube/backup"] == "true" && s.Status.Phase == "Ready" {
				backups = append(backups, s)
			}
		}
		sort.Slice(backups, func(i, j int) bool { return backups[i].Status.TakenAt.After(backups[j].Status.TakenAt) })
		for _, old := range backups[min(keep, len(backups)):] {
			if !snapshotInUse(st, old) {
				delete(st.Snapshots, old.Metadata.Key())
			}
		}
	}
}

// restorePending reports whether a volume still has to be filled from a snapshot.
func restorePending(v *api.Volume) bool {
	return v.Status.RestoreFrom != "" && v.Status.Restored != v.Status.RestoreFrom
}

// snapshotInUse reports whether a pending restore or migration needs a snapshot.
func snapshotInUse(st *store.State, s *api.VolumeSnapshot) bool {
	for _, v := range st.Volumes {
		if v.Metadata.Namespace != s.Metadata.Namespace {
			continue
		}
		if (restorePending(v) && v.Status.RestoreFrom == s.Metadata.Name) ||
			(v.Status.Migration != nil && v.Status.Migration.Snapshot == s.Metadata.Name) {
			return true
		}
	}
	return false
}

// latestSnapshot returns the newest Ready snapshot of a volume.
func latestSnapshot(st *store.State, v *api.Volume) *api.VolumeSnapshot {
	var best *api.VolumeSnapshot
	for _, s := range st.Snapshots {
		if s.Metadata.Namespace == v.Metadata.Namespace && s.Spec.Volume == v.Metadata.Name && s.Status.Phase == "Ready" &&
			(best == nil || s.Status.TakenAt.After(best.Status.TakenAt)) {
			best = s
		}
	}
	return best
}

func volumeBusy(st *store.State, ns, name string) bool {
	for _, p := range st.Pods {
		if p.Metadata.Namespace != ns || p.Spec.NodeName == "" {
			continue
		}
		for _, v := range p.Spec.Template.AllVolumeMounts() {
			if v.Volume == name {
				return true
			}
		}
	}
	return false
}

// reconcileMigrations moves local volumes between nodes (on request, or to
// recover from a lost node) by snapshot + restore.
func reconcileMigrations(st *store.State, now time.Time, seen func(*api.Node) time.Time) {
	for _, v := range st.Volumes {
		ns, name := v.Metadata.Namespace, v.Metadata.Name
		m := v.Status.Migration

		// Disaster recovery for local volumes on a lost node.
		if m == nil && v.Spec.AutoRecover && v.Spec.Type == "local" && v.Status.NodeName != "" {
			n := st.Nodes[v.Status.NodeName]
			lost := n == nil || (n.Status != api.NodeReady && now.Sub(seen(n)) > recoverAfter)
			if lost {
				snap := latestSnapshot(st, v)
				target := pickVolumeTarget(st, v.Status.NodeName)
				switch {
				case snap == nil:
					st.Event("Warning", api.KindVolume, ns, name, "RecoverFailed", "node lost and the volume has no snapshot to restore")
				case target == "":
					st.Event("Warning", api.KindVolume, ns, name, "RecoverFailed", "node lost and no other node is ready")
				default:
					st.Event("Warning", api.KindVolume, ns, name, "Recovering",
						fmt.Sprintf("node %s lost: restoring snapshot %s (taken %s) on %s", v.Status.NodeName, snap.Metadata.Name, snap.Status.TakenAt.Format(time.RFC3339), target))
					v.Status.NodeName, v.Status.RestoreFrom, v.Status.Restored = target, snap.Metadata.Name, ""
					v.Status.Migration = &api.VolumeMigration{Target: target, Phase: "Restoring", Snapshot: snap.Metadata.Name, Started: now, Reason: "recover"}
				}
			}
			continue
		}
		if m == nil {
			continue
		}
		switch m.Phase {
		case "Draining":
			for _, p := range st.Pods {
				if p.Metadata.Namespace != ns {
					continue
				}
				for _, vm := range p.Spec.Template.AllVolumeMounts() {
					if vm.Volume == name && p.Status.Phase != api.PodTerminating {
						terminatePod(st, p, false)
					}
				}
			}
			if !volumeBusy(st, ns, name) {
				snap := createSnapshot(st, ns, fmt.Sprintf("%s-move-%s", name, now.UTC().Format("20060102-150405")), name, map[string]string{"nube/migration": "true"})
				m.Snapshot, m.Phase, m.Message = snap.Metadata.Name, "Snapshotting", "copying the data off "+v.Status.NodeName
			} else {
				m.Message = "stopping the pods that use the volume"
			}
		case "Snapshotting":
			s := st.Snapshots[ns+"/"+m.Snapshot]
			switch {
			case s == nil || s.Status.Phase == "Failed":
				m.Phase = "Failed"
				m.Message = "snapshot failed"
				if s != nil {
					m.Message += ": " + s.Status.Message
				}
				st.Event("Warning", api.KindVolume, ns, name, "MoveFailed", m.Message+"; the volume stays on "+v.Status.NodeName)
			case s.Status.Phase == "Ready":
				v.Status.NodeName, v.Status.RestoreFrom, v.Status.Restored = m.Target, m.Snapshot, ""
				m.Phase, m.Message = "Restoring", "restoring on "+m.Target
			}
		case "Restoring":
			if v.Status.Restored == v.Status.RestoreFrom {
				st.Event("Normal", api.KindVolume, ns, name, "Moved", "volume is now on "+m.Target)
				v.Status.Migration = nil
			} else {
				m.Message = "waiting for a pod on " + m.Target + " (data is restored before it starts)"
			}
		case "Failed":
			if now.Sub(m.Started) > 10*time.Minute {
				v.Status.Migration = nil
			}
		}
	}
}

// pickVolumeTarget chooses a ready node other than exclude with the most free memory.
func pickVolumeTarget(st *store.State, exclude string) string {
	best, bestFree := "", int64(-1)
	for name, n := range st.Nodes {
		if name == exclude || n.Status != api.NodeReady || n.Cordoned {
			continue
		}
		if free := n.Capacity.Memory - n.Allocated.Memory; free > bestFree {
			best, bestFree = name, free
		}
	}
	return best
}

// ---------------------------------------------------------------- HTTP

func (s *Server) snapshotDir() string { return filepath.Join(s.cfg.DataDir, "snapshots") }

func (s *Server) snapshotPath(vs *api.VolumeSnapshot) string {
	return filepath.Join(s.snapshotDir(), vs.Metadata.Namespace, vs.Metadata.Name+"-"+vs.Metadata.UID+".tar.gz")
}

func (s *Server) serverID() string {
	if s.cfg.ServerID != "" {
		return s.cfg.ServerID
	}
	return "server"
}

func (s *Server) handleVolumeMove(w http.ResponseWriter, r *http.Request) {
	ns, name := r.PathValue("ns"), r.PathValue("name")
	var body struct {
		Node string `json:"node"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Node == "" {
		writeErr(w, 400, "body must be {\"node\": \"target-node\"}")
		return
	}
	err := s.store.Write(func(st *store.State) error {
		v := st.Volumes[ns+"/"+name]
		if v == nil {
			return notFound("volume/" + name)
		}
		if v.Spec.Type != "local" {
			return fmt.Errorf("only local volumes are bound to a node (%s volumes are reachable from every node)", v.Spec.Type)
		}
		n := st.Nodes[body.Node]
		if n == nil || n.Status != api.NodeReady {
			return fmt.Errorf("node %s is not ready", body.Node)
		}
		if v.Status.Migration != nil && v.Status.Migration.Phase != "Failed" {
			return fmt.Errorf("volume is already being moved to %s", v.Status.Migration.Target)
		}
		if v.Status.NodeName == body.Node {
			return fmt.Errorf("volume is already on %s", body.Node)
		}
		if v.Status.NodeName == "" {
			v.Spec.NodeName = body.Node // never used: just bind it there
			v.Status.NodeName, v.Status.Phase = body.Node, "Bound"
			return nil
		}
		v.Status.Migration = &api.VolumeMigration{Target: body.Node, Phase: "Draining", Started: time.Now(), Reason: "move"}
		st.Event("Normal", api.KindVolume, ns, name, "MoveStarted", fmt.Sprintf("moving from %s to %s: pods using it are stopped until the data is copied", v.Status.NodeName, body.Node))
		return nil
	})
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	writeJSON(w, 200, map[string]string{"status": "moving volume " + name + " to " + body.Node})
}

// handleSnapshotUpload receives a snapshot archive from an agent (leader only:
// it is a POST, forwarded to the leader by the middleware).
func (s *Server) handleSnapshotUpload(w http.ResponseWriter, r *http.Request) {
	ns, name := r.PathValue("ns"), r.PathValue("name")
	var vs *api.VolumeSnapshot
	s.store.Read(func(st *store.State) {
		if x := st.Snapshots[ns+"/"+name]; x != nil {
			c := *x
			vs = &c
		}
	})
	if vs == nil {
		writeErr(w, 404, "snapshot not found")
		return
	}
	if vs.Status.Phase == "Ready" {
		io.Copy(io.Discard, r.Body)
		writeJSON(w, 200, map[string]string{"status": "already stored"})
		return
	}
	if r.Header.Get("X-Nube-Error") != "" {
		s.store.Write(func(st *store.State) error {
			if x := st.Snapshots[ns+"/"+name]; x != nil {
				x.Status.Phase, x.Status.Message = "Failed", r.Header.Get("X-Nube-Error")
				st.Event("Warning", api.KindVolumeSnapshot, ns, name, "Failed", x.Status.Message)
			}
			return nil
		})
		writeJSON(w, 200, map[string]string{"status": "recorded failure"})
		return
	}
	path := s.snapshotPath(vs)
	size, sum, err := saveFile(path, r.Body)
	if err != nil {
		writeErr(w, 500, "store snapshot: "+err.Error())
		return
	}
	err = s.store.Write(func(st *store.State) error {
		x := st.Snapshots[ns+"/"+name]
		if x == nil {
			return notFound("volumesnapshot/" + name)
		}
		x.Status.Phase, x.Status.SizeBytes, x.Status.SHA256, x.Status.TakenAt = "Ready", size, sum, time.Now()
		x.Status.StoredOn, x.Status.Message = []string{s.serverID()}, ""
		st.Event("Normal", api.KindVolumeSnapshot, ns, name, "Ready", fmt.Sprintf("snapshot stored (%s)", humanBytes(size)))
		return nil
	})
	if err != nil {
		os.Remove(path)
		writeStoreErr(w, err)
		return
	}
	writeJSON(w, 200, map[string]string{"status": "stored"})
}

func saveFile(path string, r io.Reader) (int64, string, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return 0, "", err
	}
	tmp := path + ".part"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return 0, "", err
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, h), r)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(tmp)
		return 0, "", err
	}
	return n, hex.EncodeToString(h.Sum(nil)), os.Rename(tmp, path)
}

func humanBytes(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GiB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KiB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}

// handleSnapshotDownload serves a snapshot archive from this server, or
// proxies to a server that has it.
func (s *Server) handleSnapshotDownload(w http.ResponseWriter, r *http.Request) {
	ns, name := r.PathValue("ns"), r.PathValue("name")
	var vs *api.VolumeSnapshot
	holders := map[string]string{}
	s.store.Read(func(st *store.State) {
		if x := st.Snapshots[ns+"/"+name]; x != nil {
			c := *x
			vs = &c
			for _, id := range x.Status.StoredOn {
				if m := st.Servers[id]; m != nil {
					holders[id] = m.APIAddr
				}
			}
		}
	})
	if vs == nil || vs.Status.Phase != "Ready" {
		writeErr(w, 404, "snapshot not found or not ready")
		return
	}
	if f, err := os.Open(s.snapshotPath(vs)); err == nil {
		defer f.Close()
		w.Header().Set("Content-Type", "application/gzip")
		w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s.tar.gz"`, name))
		w.Header().Set("X-Nube-SHA256", vs.Status.SHA256)
		io.Copy(w, f)
		return
	}
	if r.Header.Get("X-Nube-Forwarded") == "" {
		for id, addr := range holders {
			if id == s.serverID() || addr == "" {
				continue
			}
			req, _ := http.NewRequestWithContext(r.Context(), "GET", strings.TrimRight(addr, "/")+r.URL.Path, nil)
			req.Header.Set("Authorization", "Bearer "+s.cfg.Token)
			req.Header.Set("X-Nube-Forwarded", s.serverID())
			resp, err := s.clusterClient(0).Do(req)
			if err != nil || resp.StatusCode != 200 {
				if resp != nil {
					resp.Body.Close()
				}
				continue
			}
			defer resp.Body.Close()
			w.Header().Set("Content-Type", "application/gzip")
			io.Copy(w, resp.Body)
			return
		}
	}
	writeErr(w, 503, "no reachable server holds this snapshot")
}

// snapshotLoop copies Ready snapshots to this server (so every server has
// every backup) and deletes local files of deleted snapshots.
func (s *Server) snapshotLoop(ctx context.Context) {
	t := time.NewTicker(20 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		var missing []*api.VolumeSnapshot
		keep := map[string]bool{}
		s.store.Read(func(st *store.State) {
			for _, vs := range st.Snapshots {
				keep[s.snapshotPath(vs)] = true
				if vs.Status.Phase == "Ready" && !contains(vs.Status.StoredOn, s.serverID()) {
					c := *vs
					missing = append(missing, &c)
				}
			}
		})
		for _, vs := range missing {
			path := s.snapshotPath(vs)
			if _, err := os.Stat(path); err != nil {
				if err := s.fetchSnapshot(ctx, vs, path); err != nil {
					log.Printf("snapshots: copy %s/%s: %v", vs.Metadata.Namespace, vs.Metadata.Name, err)
					continue
				}
			}
			s.reportStored(vs.Metadata.Namespace, vs.Metadata.Name)
		}
		filepath.Walk(s.snapshotDir(), func(p string, info os.FileInfo, err error) error {
			if err == nil && !info.IsDir() && strings.HasSuffix(p, ".tar.gz") && !keep[p] && time.Since(info.ModTime()) > time.Minute {
				os.Remove(p)
			}
			return nil
		})
	}
}

func (s *Server) fetchSnapshot(ctx context.Context, vs *api.VolumeSnapshot, path string) error {
	url := s.localAPI() + "/api/v1/namespaces/" + vs.Metadata.Namespace + "/volumesnapshots/" + vs.Metadata.Name + "/data"
	req, _ := http.NewRequestWithContext(ctx, "GET", url, nil)
	req.Header.Set("Authorization", "Bearer "+s.cfg.Token)
	resp, err := s.clusterClient(0).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	_, sum, err := saveFile(path, resp.Body)
	if err != nil {
		return err
	}
	if sum != vs.Status.SHA256 {
		os.Remove(path)
		return fmt.Errorf("checksum mismatch")
	}
	return nil
}

// localAPI is this server's own API URL (requests to it proxy to holders).
func (s *Server) localAPI() string {
	if s.cfg.APIAdvertise != "" {
		return strings.TrimRight(s.cfg.APIAdvertise, "/")
	}
	port := strings.TrimPrefix(s.cfg.Listen, ":")
	if i := strings.LastIndex(s.cfg.Listen, ":"); i >= 0 {
		port = s.cfg.Listen[i+1:]
	}
	return s.scheme() + "://127.0.0.1:" + port
}

// reportStored records that this server holds a copy (through the leader).
func (s *Server) reportStored(ns, name string) {
	mark := func(st *store.State) error {
		if x := st.Snapshots[ns+"/"+name]; x != nil && !contains(x.Status.StoredOn, s.serverID()) {
			x.Status.StoredOn = append(x.Status.StoredOn, s.serverID())
		}
		return nil
	}
	if s.store.IsLeader() {
		s.store.Write(mark)
		return
	}
	body, _ := json.Marshal(map[string]string{"namespace": ns, "name": name, "server": s.serverID()})
	s.leaderPost("/api/v1/cluster/snapshots/stored", body)
}

func (s *Server) handleSnapshotStored(w http.ResponseWriter, r *http.Request) {
	var b struct{ Namespace, Name, Server string }
	if err := json.NewDecoder(r.Body).Decode(&b); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	err := s.store.Write(func(st *store.State) error {
		if x := st.Snapshots[b.Namespace+"/"+b.Name]; x != nil && !contains(x.Status.StoredOn, b.Server) {
			x.Status.StoredOn = append(x.Status.StoredOn, b.Server)
		}
		return nil
	})
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	writeJSON(w, 200, map[string]string{"status": "ok"})
}

// leaderPost sends an internal request to the leader with the admin token.
func (s *Server) leaderPost(path string, body []byte) error {
	leader := s.store.LeaderAPI()
	if leader == "" {
		return fmt.Errorf("no leader")
	}
	req, _ := http.NewRequest("POST", strings.TrimRight(leader, "/")+path, strings.NewReader(string(body)))
	req.Header.Set("Authorization", "Bearer "+s.cfg.Token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.clusterClient(30 * time.Second).Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("leader answered %s", resp.Status)
	}
	return nil
}

// snapshotTasks lists the snapshots a node must take now.
func snapshotTasks(st *store.State, node string) []api.SnapshotTask {
	var out []api.SnapshotTask
	for _, vs := range st.Snapshots {
		if vs.Status.Phase != "Pending" || vs.Status.Node != node {
			continue
		}
		if v := st.Volumes[vs.Metadata.Namespace+"/"+vs.Spec.Volume]; v != nil {
			out = append(out, api.SnapshotTask{Namespace: vs.Metadata.Namespace, Snapshot: vs.Metadata.Name, Volume: *v})
		}
	}
	return out
}

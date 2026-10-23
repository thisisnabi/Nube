package agent

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/thisisnabi/Nube/internal/api"
)

const restoreMarker = ".nube-restored"

// volumeDataPath returns the host directory holding a volume's data, mounting
// it if needed (and an unmount function for temporary mounts).
func (a *Agent) volumeDataPath(ctx context.Context, v api.Volume) (string, func(), error) {
	nop := func() {}
	switch v.Spec.Type {
	case "hostPath":
		return v.Spec.HostPath, nop, nil
	case "nfs":
		if v.Spec.AccessMode == "ReadWriteOnce" {
			size, _ := api.ParseMemory(v.Spec.Size)
			share, err := a.mountNFSShare(v.Spec.NFS)
			if err != nil {
				return "", nop, err
			}
			p, err := a.sized.EnsureIn(share, v.DockerName(), size, true)
			return p, nop, err
		}
		share, err := a.mountNFSShare(v.Spec.NFS)
		return share, nop, err
	}
	// local
	if a.docker.VolumeExists(ctx, v.DockerName()) {
		mp, err := a.docker.VolumeMountpoint(ctx, v.DockerName())
		return mp, nop, err
	}
	size, _ := api.ParseMemory(v.Spec.Size)
	if size > 0 {
		p, err := a.sized.Ensure(v.DockerName(), size)
		return p, nop, err
	}
	if err := a.docker.EnsureVolume(ctx, v.DockerName(), nil, nil); err != nil {
		return "", nop, err
	}
	mp, err := a.docker.VolumeMountpoint(ctx, v.DockerName())
	return mp, nop, err
}

// runSnapshots handles snapshot tasks from the server (one at a time per snapshot).
func (a *Agent) runSnapshots(tasks []api.SnapshotTask) {
	for _, t := range tasks {
		key := t.Namespace + "/" + t.Snapshot
		a.mu.Lock()
		busy := a.snapBusy[key]
		if !busy {
			a.snapBusy[key] = true
		}
		a.mu.Unlock()
		if busy {
			continue
		}
		go func(t api.SnapshotTask) {
			defer func() {
				// Allow a retry later only if the server still wants it.
				time.Sleep(time.Minute)
				a.mu.Lock()
				delete(a.snapBusy, key)
				a.mu.Unlock()
			}()
			if err := a.takeSnapshot(t); err != nil {
				log.Printf("snapshot %s: %v", key, err)
				a.reportSnapshotError(t, err)
			} else {
				log.Printf("snapshot %s uploaded", key)
			}
		}(t)
	}
}

func (a *Agent) takeSnapshot(t api.SnapshotTask) error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Hour)
	defer cancel()
	dir, done, err := a.volumeDataPath(ctx, t.Volume)
	if err != nil {
		return err
	}
	defer done()
	pr, pw := io.Pipe()
	go func() { pw.CloseWithError(writeTarGz(pw, dir)) }()
	url := a.cfg.Servers[a.server] + "/api/v1/agent/snapshots/" + t.Namespace + "/" + t.Snapshot
	req, _ := http.NewRequestWithContext(ctx, "POST", url, pr)
	req.Header.Set("Authorization", "Bearer "+a.cfg.Token)
	req.Header.Set("Content-Type", "application/gzip")
	resp, err := a.streamClient().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("upload: %s: %s", resp.Status, strings.TrimSpace(string(b)))
	}
	return nil
}

func (a *Agent) reportSnapshotError(t api.SnapshotTask, cause error) {
	url := a.cfg.Servers[a.server] + "/api/v1/agent/snapshots/" + t.Namespace + "/" + t.Snapshot
	req, _ := http.NewRequest("POST", url, nil)
	req.Header.Set("Authorization", "Bearer "+a.cfg.Token)
	req.Header.Set("X-Nube-Error", strings.ReplaceAll(cause.Error(), "\n", " "))
	if resp, err := a.http.Do(req); err == nil {
		resp.Body.Close()
	}
}

// streamClient is an HTTP client without a timeout, for large transfers.
func (a *Agent) streamClient() *http.Client {
	return &http.Client{Transport: a.http.Transport}
}

// restoreInto fills dir from a snapshot unless that snapshot was already restored there.
func (a *Agent) restoreInto(ctx context.Context, v api.Volume, dir string) error {
	snap := v.Status.RestoreFrom
	if snap == "" || v.Status.Restored == snap {
		return nil
	}
	marker := filepath.Join(dir, restoreMarker)
	if b, err := os.ReadFile(marker); err == nil && strings.TrimSpace(string(b)) == snap {
		a.markRestored(v, snap)
		return nil
	}
	if err := a.setAsideStale(v, dir); err != nil {
		return err
	}
	url := a.cfg.Servers[a.server] + "/api/v1/agent/snapshots/" + v.Metadata.Namespace + "/" + snap
	req, _ := http.NewRequestWithContext(ctx, "GET", url, nil)
	req.Header.Set("Authorization", "Bearer "+a.cfg.Token)
	resp, err := a.streamClient().Do(req)
	if err != nil {
		return fmt.Errorf("download snapshot %s: %w", snap, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("download snapshot %s: %s", snap, resp.Status)
	}
	if err := extractTarGz(resp.Body, dir); err != nil {
		return fmt.Errorf("restore snapshot %s: %w", snap, err)
	}
	if err := os.WriteFile(marker, []byte(snap+"\n"), 0o600); err != nil {
		return err
	}
	log.Printf("volume %s/%s restored from snapshot %s", v.Metadata.Namespace, v.Metadata.Name, snap)
	a.markRestored(v, snap)
	return nil
}

// setAsideStale handles a target that already holds data, e.g. an old copy
// left on this node by an earlier move. That copy is outdated (the snapshot
// is newer), but it is moved aside rather than deleted:
//   - sized volumes: the image file is renamed to <name>.stale-<time>.img
//     and a fresh one is created;
//   - Docker volumes: the contents move to <volume>/stale-<time>/, next to _data.
func (a *Agent) setAsideStale(v api.Volume, dir string) error {
	entries, _ := os.ReadDir(dir)
	var stale []string
	for _, e := range entries {
		if e.Name() != "lost+found" && e.Name() != restoreMarker {
			stale = append(stale, e.Name())
		}
	}
	if len(stale) == 0 {
		return nil
	}
	if v.Spec.Type != "local" {
		return fmt.Errorf("volume %s is not empty; refusing to overwrite it with snapshot %s", v.Metadata.Name, v.Status.RestoreFrom)
	}
	stamp := time.Now().UTC().Format("20060102-150405")
	if filepath.Dir(dir) == filepath.Clean(a.sized.dir) && isMounted(dir) {
		name := filepath.Base(dir)
		img := filepath.Join(a.sized.dir, name+".img")
		if err := a.sized.Unmount(name); err != nil {
			return err
		}
		aside := filepath.Join(a.sized.dir, name+".stale-"+stamp+".img")
		if err := os.Rename(img, aside); err != nil {
			return err
		}
		size, _ := api.ParseMemory(v.Spec.Size)
		if _, err := a.sized.Ensure(name, size); err != nil {
			return err
		}
		log.Printf("volume %s: outdated local copy kept as %s", v.Metadata.Name, aside)
		return nil
	}
	aside := filepath.Join(filepath.Dir(dir), "stale-"+stamp)
	if err := os.MkdirAll(aside, 0o700); err != nil {
		return err
	}
	for _, n := range stale {
		if err := os.Rename(filepath.Join(dir, n), filepath.Join(aside, n)); err != nil {
			return fmt.Errorf("set aside outdated data of volume %s: %w", v.Metadata.Name, err)
		}
	}
	log.Printf("volume %s: outdated local data kept in %s", v.Metadata.Name, aside)
	return nil
}

func (a *Agent) markRestored(v api.Volume, snap string) {
	a.mu.Lock()
	a.restored[v.Metadata.Namespace+"/"+v.Metadata.Name] = snap
	a.mu.Unlock()
}

// writeTarGz archives dir (contents only) with modes, owners and symlinks.
func writeTarGz(w io.Writer, dir string) error {
	gz := gzip.NewWriter(w)
	tw := tar.NewWriter(gz)
	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, path)
		if rel == "." || rel == "lost+found" || rel == restoreMarker {
			if info.IsDir() && rel == "lost+found" {
				return filepath.SkipDir
			}
			return nil
		}
		link := ""
		if info.Mode()&os.ModeSymlink != 0 {
			link, _ = os.Readlink(path)
		}
		hdr, err := tar.FileInfoHeader(info, link)
		if err != nil {
			return err
		}
		hdr.Name = filepath.ToSlash(rel)
		fillOwner(hdr, info)
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if info.Mode().IsRegular() {
			f, err := os.Open(path)
			if err != nil {
				return err
			}
			_, err = io.Copy(tw, f)
			f.Close()
			return err
		}
		return nil
	})
	if err != nil {
		return err
	}
	if err := tw.Close(); err != nil {
		return err
	}
	return gz.Close()
}

// extractTarGz unpacks an archive into dir, refusing paths that escape it.
func extractTarGz(r io.Reader, dir string) error {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return err
	}
	tr := tar.NewReader(gz)
	root := filepath.Clean(dir) + string(os.PathSeparator)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		target := filepath.Join(dir, filepath.FromSlash(hdr.Name))
		if !strings.HasPrefix(target+string(os.PathSeparator), root) {
			return fmt.Errorf("unsafe path %q in archive", hdr.Name)
		}
		mode := os.FileMode(hdr.Mode).Perm()
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, mode|0o700); err != nil {
				return err
			}
		case tar.TypeReg:
			os.MkdirAll(filepath.Dir(target), 0o755)
			f, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
			if err != nil {
				return err
			}
			_, err = io.Copy(f, tr)
			f.Close()
			if err != nil {
				return err
			}
		case tar.TypeSymlink:
			os.MkdirAll(filepath.Dir(target), 0o755)
			if err := os.Symlink(hdr.Linkname, target); err != nil && !os.IsExist(err) {
				return err
			}
		default:
			continue
		}
		os.Lchown(target, hdr.Uid, hdr.Gid)
		if hdr.Typeflag != tar.TypeSymlink {
			os.Chmod(target, mode)
			os.Chtimes(target, hdr.ModTime, hdr.ModTime)
		}
	}
}

// mountNFSShare mounts an NFS export under the agent's data dir (once).
func (a *Agent) mountNFSShare(n *api.NFSSource) (string, error) {
	if n == nil {
		return "", errors.New("nfs source missing")
	}
	dir := filepath.Join(a.cfg.DataDir, "nfs", sanitize(n.Server+"_"+n.Path))
	if isMounted(dir) {
		return dir, nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	args := []string{"-t", "nfs"}
	if n.Options != "" {
		args = append(args, "-o", n.Options)
	}
	args = append(args, n.Server+":"+n.Path, dir)
	if out, err := exec.Command("mount", args...).CombinedOutput(); err != nil {
		return "", fmt.Errorf("mount nfs %s:%s: %v: %s", n.Server, n.Path, err, strings.TrimSpace(string(out)))
	}
	return dir, nil
}

func sanitize(s string) string {
	return strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '.' {
			return r
		}
		return '_'
	}, s)
}

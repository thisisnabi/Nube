package agent

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
)

// sizedVolumes provides directories with a hard size limit: each one is an
// ext4 filesystem in its own image file, loop-mounted under <data>/volumes.
// Writing past the size fails with ENOSPC inside the container, exactly like
// a full disk.
type sizedVolumes struct {
	dir string
	mu  sync.Mutex
}

func newSizedVolumes(dataDir string) *sizedVolumes {
	return &sizedVolumes{dir: filepath.Join(dataDir, "volumes")}
}

// supported reports whether this host can create sized volumes.
func (v *sizedVolumes) supported() error {
	for _, tool := range []string{"mkfs.ext4", "mount", "losetup", "resize2fs"} {
		if _, err := exec.LookPath(tool); err != nil {
			return fmt.Errorf("%s not found (apt install e2fsprogs util-linux mount)", tool)
		}
	}
	return nil
}

// Ensure returns the mount path of the volume, creating, mounting or growing it.
func (v *sizedVolumes) Ensure(name string, sizeMiB int64) (string, error) {
	return v.EnsureIn(v.dir, name, sizeMiB, false)
}

// EnsureIn keeps the image file in imgDir (e.g. an NFS share) while mounting
// it locally. shared=true enables ext4 multi-mount protection, so a second
// node cannot mount the same image while it is in use.
func (v *sizedVolumes) EnsureIn(imgDir, name string, sizeMiB int64, shared bool) (string, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if err := v.supported(); err != nil {
		return "", err
	}
	if err := os.MkdirAll(v.dir, 0o700); err != nil {
		return "", err
	}
	img := filepath.Join(imgDir, name+".img")
	mnt := filepath.Join(v.dir, name)
	want := imageBytes(sizeMiB)

	st, err := os.Stat(img)
	switch {
	case errors.Is(err, os.ErrNotExist):
		f, err := os.OpenFile(img, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
		if err != nil {
			return "", err
		}
		err = f.Truncate(want) // sparse: only used blocks take disk space
		f.Close()
		if err != nil {
			os.Remove(img)
			return "", err
		}
		// -m 0: no root-reserved blocks, the whole size is usable.
		journal := min(max(sizeMiB/64, 4), 64)
		args := []string{"-q", "-F", "-m", "0", "-i", "32768", "-J", fmt.Sprintf("size=%d", journal), "-L", trimLabel(name)}
		if shared {
			args = append(args, "-O", "mmp")
		}
		args = append(args, img)
		if out, err := exec.Command("mkfs.ext4", args...).CombinedOutput(); err != nil {
			os.Remove(img)
			return "", fmt.Errorf("mkfs.ext4: %v: %s", err, strings.TrimSpace(string(out)))
		}
	case err != nil:
		return "", err
	case st.Size() > want:
		return "", fmt.Errorf("volume %s is larger than %d MiB; shrinking is not supported", name, sizeMiB)
	}

	if err := os.MkdirAll(mnt, 0o755); err != nil {
		return "", err
	}
	if !isMounted(mnt) {
		if shared {
			// The previous node may have died with it mounted: repair first.
			// With multi-mount protection this fails if it is mounted elsewhere.
			if out, err := exec.Command("e2fsck", "-p", img).CombinedOutput(); err != nil {
				if ee, ok := err.(*exec.ExitError); !ok || ee.ExitCode() > 1 {
					return "", fmt.Errorf("e2fsck %s (in use on another node?): %v: %s", img, err, strings.TrimSpace(string(out)))
				}
			}
		}
		if out, err := exec.Command("mount", "-o", "loop", img, mnt).CombinedOutput(); err != nil {
			return "", fmt.Errorf("mount %s: %v: %s", img, err, strings.TrimSpace(string(out)))
		}
	}
	// Grow online when the requested size increased.
	if st != nil && st.Size() < want {
		if err := os.Truncate(img, want); err != nil {
			return "", err
		}
		if dev := loopDevice(img); dev != "" {
			exec.Command("losetup", "-c", dev).Run()
			if out, err := exec.Command("resize2fs", dev).CombinedOutput(); err != nil {
				return "", fmt.Errorf("resize2fs: %v: %s", err, strings.TrimSpace(string(out)))
			}
		}
	}
	return mnt, nil
}

// Unmount releases a mounted volume without deleting it.
func (v *sizedVolumes) Unmount(name string) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	mnt := filepath.Join(v.dir, name)
	if !isMounted(mnt) {
		return nil
	}
	if out, err := exec.Command("umount", mnt).CombinedOutput(); err != nil {
		return fmt.Errorf("umount %s: %v: %s", mnt, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// Remove unmounts and deletes a volume (used for pod-scoped emptyDirs).
func (v *sizedVolumes) Remove(name string) {
	v.mu.Lock()
	defer v.mu.Unlock()
	mnt := filepath.Join(v.dir, name)
	if isMounted(mnt) {
		exec.Command("umount", mnt).Run()
	}
	os.Remove(mnt)
	os.Remove(filepath.Join(v.dir, name+".img"))
}

// Path returns where a volume is (or would be) mounted.
func (v *sizedVolumes) Path(name string) string { return filepath.Join(v.dir, name) }

func trimLabel(s string) string {
	if len(s) > 16 {
		return s[len(s)-16:]
	}
	return s
}

func isMounted(path string) bool {
	f, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return false
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) > 4 && unescapeMount(fields[4]) == path {
			return true
		}
	}
	return false
}

func unescapeMount(s string) string {
	return strings.NewReplacer(`\040`, " ", `\011`, "\t", `\012`, "\n", `\134`, `\`).Replace(s)
}

func loopDevice(img string) string {
	out, err := exec.Command("losetup", "-j", img).Output()
	if err != nil {
		return ""
	}
	dev, _, _ := strings.Cut(strings.TrimSpace(string(out)), ":")
	return dev
}

// imageBytes sizes the backing file so the usable space is at least the
// requested size: ext4 needs room for its journal and metadata.
func imageBytes(sizeMiB int64) int64 {
	journal := min(max(sizeMiB/64, 4), 64)
	overhead := journal + sizeMiB*3/100 + 4
	return (sizeMiB + overhead) << 20
}

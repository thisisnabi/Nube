package agent

import (
	"archive/tar"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"sync"
)

// The pause container owns a pod's network namespace (IP, ports, DNS,
// hostname); the pod's other containers join it, so they share localhost.
// Its image is built locally from this very binary ("nube pause" just sleeps),
// which means no registry access is needed for it.

var (
	pauseOnce sync.Once
	pauseRef  string
	pauseErr  error
	pauseMu   sync.Mutex
)

func pauseImageRef() (string, error) {
	pauseOnce.Do(func() {
		exe, err := os.Executable()
		if err != nil {
			pauseErr = err
			return
		}
		f, err := os.Open(exe)
		if err != nil {
			pauseErr = err
			return
		}
		defer f.Close()
		h := sha256.New()
		io.Copy(h, f)
		pauseRef = "nube/pause:" + hex.EncodeToString(h.Sum(nil))[:12]
	})
	return pauseRef, pauseErr
}

// ensurePauseImage imports the pause image if it is not present yet.
func (a *Agent) ensurePauseImage(ctx context.Context) (string, error) {
	ref, err := pauseImageRef()
	if err != nil {
		return "", err
	}
	pauseMu.Lock()
	defer pauseMu.Unlock()
	if a.docker.ImageExists(ctx, ref) {
		return ref, nil
	}
	exe, _ := os.Executable()
	f, err := os.Open(exe)
	if err != nil {
		return "", err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return "", err
	}
	pr, pw := io.Pipe()
	go func() {
		tw := tar.NewWriter(pw)
		err := tw.WriteHeader(&tar.Header{Name: "nube", Mode: 0o755, Size: st.Size(), Typeflag: tar.TypeReg})
		if err == nil {
			_, err = io.Copy(tw, f)
		}
		if err == nil {
			err = tw.Close()
		}
		pw.CloseWithError(err)
	}()
	if err := a.docker.ImportImage(ctx, ref, []string{`CMD ["/nube", "pause"]`}, pr); err != nil {
		return "", fmt.Errorf("build pause image: %w", err)
	}
	return ref, nil
}

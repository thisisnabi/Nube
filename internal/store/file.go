package store

import (
	"bytes"
	"encoding/json"
	"errors"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/thisisnabi/Nube/internal/api"
)

// FileStore keeps state in memory and flushes it to a JSON file (single-server mode).
type FileStore struct {
	mu       sync.RWMutex
	state    *State
	path     string
	dirty    bool
	lastSave []byte
	stop     chan struct{}
	done     chan struct{}
}

func NewFileStore(dir string) (*FileStore, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	fs := &FileStore{path: filepath.Join(dir, "state.json"), stop: make(chan struct{}), done: make(chan struct{})}
	b, err := os.ReadFile(fs.path)
	switch {
	case err == nil:
		st, err := decodeState(b)
		if err != nil {
			return nil, err
		}
		fs.state, fs.lastSave = st, b
	case errors.Is(err, os.ErrNotExist):
		fs.state = NewState()
	default:
		return nil, err
	}
	go fs.flushLoop()
	return fs, nil
}

func (f *FileStore) Read(fn func(*State)) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	fn(f.state)
}

func (f *FileStore) Write(fn func(*State) error) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := fn(f.state); err != nil {
		return err
	}
	f.dirty = true
	return nil
}

func (f *FileStore) flushLoop() {
	defer close(f.done)
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			f.flush()
		case <-f.stop:
			f.flush()
			return
		}
	}
}

func (f *FileStore) flush() {
	f.mu.Lock()
	if !f.dirty {
		f.mu.Unlock()
		return
	}
	b, err := json.Marshal(f.state)
	f.dirty = false
	f.mu.Unlock()
	if err != nil || bytes.Equal(b, f.lastSave) {
		return
	}
	tmp := f.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		log.Printf("store: write: %v", err)
		return
	}
	if err := os.Rename(tmp, f.path); err != nil {
		log.Printf("store: rename: %v", err)
		return
	}
	f.lastSave = b
}

func (f *FileStore) IsLeader() bool    { return true }
func (f *FileStore) LeaderAPI() string { return "" }
func (f *FileStore) Cluster() api.ClusterInfo {
	return api.ClusterInfo{Mode: "single", ServerID: "server", Leader: "server", IsLeader: true, State: "Leader"}
}
func (f *FileStore) AddServer(string, string, string) error {
	return errors.New("this server runs in single mode; restart it with --ha to form a cluster")
}
func (f *FileStore) RemoveServer(string) error { return errors.New("single mode") }

func (f *FileStore) Close() error {
	close(f.stop)
	<-f.done
	return nil
}

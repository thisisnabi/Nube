package agent

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTarRoundTrip(t *testing.T) {
	src := t.TempDir()
	os.MkdirAll(filepath.Join(src, "a", "b"), 0o755)
	os.WriteFile(filepath.Join(src, "a", "b", "file.txt"), []byte("hello"), 0o640)
	os.WriteFile(filepath.Join(src, "top.bin"), bytes.Repeat([]byte{7}, 100000), 0o600)
	os.WriteFile(filepath.Join(src, restoreMarker), []byte("old"), 0o600) // never archived

	var buf bytes.Buffer
	if err := writeTarGz(&buf, src); err != nil {
		t.Fatal(err)
	}
	dst := t.TempDir()
	if err := extractTarGz(&buf, dst); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(dst, "a", "b", "file.txt"))
	if err != nil || string(b) != "hello" {
		t.Fatalf("file.txt = %q, %v", b, err)
	}
	if st, _ := os.Stat(filepath.Join(dst, "top.bin")); st == nil || st.Size() != 100000 {
		t.Fatal("top.bin missing or wrong size")
	}
	if _, err := os.Stat(filepath.Join(dst, restoreMarker)); err == nil {
		t.Fatal("the restore marker must not travel in snapshots")
	}
}

func TestExtractRejectsPathTraversal(t *testing.T) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	tw.WriteHeader(&tar.Header{Name: "../escape.txt", Mode: 0o644, Size: 2, Typeflag: tar.TypeReg})
	tw.Write([]byte("hi"))
	tw.Close()
	gz.Close()
	dir := t.TempDir()
	err := extractTarGz(&buf, dir)
	if err == nil || !strings.Contains(err.Error(), "unsafe path") {
		t.Fatalf("expected unsafe path error, got %v", err)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(dir), "escape.txt")); err == nil {
		t.Fatal("file escaped the target directory")
	}
}

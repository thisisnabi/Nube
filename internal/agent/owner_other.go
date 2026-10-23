//go:build !linux

package agent

import (
	"archive/tar"
	"os"
)

func fillOwner(*tar.Header, os.FileInfo) {}

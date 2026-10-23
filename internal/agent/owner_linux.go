package agent

import (
	"archive/tar"
	"os"
	"syscall"
)

func fillOwner(hdr *tar.Header, info os.FileInfo) {
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		hdr.Uid, hdr.Gid = int(st.Uid), int(st.Gid)
	}
}

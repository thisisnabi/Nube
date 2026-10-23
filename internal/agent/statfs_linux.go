package agent

import "syscall"

// diskUsage returns used and total MiB of the filesystem holding path.
func diskUsage(path string) (used, total int64, ok bool) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, 0, false
	}
	bs := int64(st.Bsize)
	total = int64(st.Blocks) * bs
	free := int64(st.Bfree) * bs
	return (total - free) >> 20, total >> 20, true
}

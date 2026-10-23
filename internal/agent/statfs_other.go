//go:build !linux

package agent

func diskUsage(string) (used, total int64, ok bool) { return 0, 0, false }

package index

import (
	"os"

	"golang.org/x/sys/unix"
)

// birth returns a file's creation time in unix ms from statx, or 0 when the
// kernel or file system does not record it. A copy of a transcript is always
// created after the original, so the earlier-born file owns the responses both
// contain.
func birth(path string, _ os.FileInfo) int64 {
	var st unix.Statx_t
	if err := unix.Statx(unix.AT_FDCWD, path, 0, unix.STATX_BTIME, &st); err != nil || st.Mask&unix.STATX_BTIME == 0 {
		return 0
	}
	return st.Btime.Sec*1000 + int64(st.Btime.Nsec)/1_000_000
}

//go:build !windows

package replay

import "syscall"

// openNoFollow refuses to open a symlink as the recording file.
const openNoFollow = syscall.O_NOFOLLOW

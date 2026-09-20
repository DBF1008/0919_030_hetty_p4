package bolt

import "syscall"

// mmapFlags uses MAP_POPULATE on Linux, which prefaults the page tables for
// the memory mapped database file, reducing page faults during reads.
const mmapFlags = syscall.MAP_POPULATE

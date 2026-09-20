package bolt

import "syscall"

// mmapFlags uses MAP_POPULATE on Linux, which pre-faults the mapped pages
// and reduces page faults when reading large database files.
const mmapFlags = syscall.MAP_POPULATE

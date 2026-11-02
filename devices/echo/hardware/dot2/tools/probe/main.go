// A disposable ARM/Linux deployment probe, without network calls or device I/O.
package main

import (
	"fmt"
	"runtime"
)

func main() {
	fmt.Printf("DOT_SOFTWARE_PROBE_OK arch=%s cores=%d go=%s\n", runtime.GOARCH, runtime.NumCPU(), runtime.Version())
}

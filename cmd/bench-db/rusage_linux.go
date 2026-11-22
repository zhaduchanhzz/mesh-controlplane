//go:build benchdb && linux

package main

import "syscall"

// cpuSeconds returns user+system CPU time consumed by this process.
func cpuSeconds() float64 {
	var ru syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &ru); err != nil {
		return 0
	}

	return float64(ru.Utime.Nano()+ru.Stime.Nano()) / 1e9
}

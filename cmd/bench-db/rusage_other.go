//go:build benchdb && !linux

package main

// cpuSeconds is only measured on Linux, where the benchmarks run.
func cpuSeconds() float64 { return 0 }

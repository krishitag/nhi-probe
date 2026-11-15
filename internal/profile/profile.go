// Package profile adds --cpuprofile / --memprofile to a command (standard runtime/pprof).
// View a profile with: go tool pprof -top nhi-probe cpu.prof
package profile

import (
	"fmt"
	"os"
	"runtime"
	"runtime/pprof"
)

// Start begins CPU profiling (if cpuPath != "") and returns stop(), which ends it.
// Call stop() even after an error.
func Start(cpuPath string) (stop func(), err error) {
	if cpuPath == "" {
		return func() {}, nil
	}
	f, err := os.Create(cpuPath)
	if err != nil {
		return func() {}, fmt.Errorf("create cpu profile: %w", err)
	}
	if err := pprof.StartCPUProfile(f); err != nil {
		f.Close()
		return func() {}, fmt.Errorf("start cpu profile: %w", err)
	}
	return func() {
		pprof.StopCPUProfile()
		f.Close()
	}, nil
}

// WriteHeap writes a heap profile now (if path != ""). Call it where memory is at
// its peak; at the very end everything is already freed and the profile is empty.
func WriteHeap(path string) {
	if path == "" {
		return
	}
	f, err := os.Create(path)
	if err != nil {
		fmt.Fprintln(os.Stderr, "nhi-probe: create mem profile:", err)
		return
	}
	defer f.Close()
	runtime.GC() // up-to-date numbers
	if err := pprof.WriteHeapProfile(f); err != nil {
		fmt.Fprintln(os.Stderr, "nhi-probe: write mem profile:", err)
	}
}

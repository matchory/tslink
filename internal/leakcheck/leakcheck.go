// Package leakcheck fails a package's tests if they leak goroutines.
package leakcheck

import (
	"bytes"
	"fmt"
	"os"
	"runtime/pprof"
	"testing"
)

// Main runs the tests and fails them if goroutines are left blocked forever:
// on a channel or lock that nothing else can reach, as the goroutineleak
// profile finds them. Goroutines waiting on a timer or on a context that is
// still reachable are not found.
func Main(m *testing.M) {
	code := m.Run()
	if leaks := Leaks(); leaks != "" && code == 0 {
		fmt.Fprint(os.Stderr, leaks)
		code = 1
	}
	os.Exit(code)
}

// Leaks returns the goroutineleak profile if it finds leaked goroutines, else
// an empty string.
func Leaks() string {
	p := pprof.Lookup("goroutineleak")
	var b bytes.Buffer
	// Writing the profile runs the detection; Count only reports its result
	if err := p.WriteTo(&b, 1); err != nil {
		return fmt.Sprintf("goroutineleak profile: %v\n", err)
	}
	if p.Count() == 0 {
		return ""
	}
	return b.String()
}

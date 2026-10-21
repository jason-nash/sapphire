package network

import (
	"testing"

	gvisorsync "gvisor.dev/gvisor/pkg/sync"
)

// The TCP workers use this primitive to park. A broken ARM64 race-build
// implementation caused idle workers to spin and starve daemon lifecycle I/O.
// Upstream fix: https://github.com/google/gvisor/commit/1b2946c4fa59c1a64643bce962140b5686062601
func TestNetworkWorkerParkCompareAndSwap(t *testing.T) {
	var value uintptr = 1
	if !gvisorsync.RaceUncheckedAtomicCompareAndSwapUintptr(&value, 1, 2) || value != 2 {
		t.Fatalf("park CAS failed to replace expected value: %d", value)
	}
	if gvisorsync.RaceUncheckedAtomicCompareAndSwapUintptr(&value, 1, 3) || value != 2 {
		t.Fatalf("park CAS changed mismatched value: %d", value)
	}
}

package lane

import (
	"sync"
	"testing"
)

// zcodePark and zcodeLevels are package-level maps touched from concurrent
// request goroutines (account rotation, roster refresh, admin card reads).
// Run this under -race: unsynchronized access is a runtime crash, not just a
// warning.
func TestZCodeStateMapsAreConcurrencySafe(t *testing.T) {
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// zcodeCool/zcodeMarkOK only touch the package map, so a zero Lane
			// is enough here.
			l := &Lane{}
			for j := 0; j < 200; j++ {
				l.zcodeCool("acct", 600)
				_ = zcodeParked("acct")
				_ = ZCodeAccountParked("acct")
				l.zcodeMarkOK("acct")
				setZcodeLevels([]zcodeConfigRow{{ModelID: "m", Levels: []string{"low"}}})
				_ = modelLevels("m")
			}
		}()
	}
	wg.Wait()
}

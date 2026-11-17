package fleet

import (
	"testing"
	"time"
)

func TestIntervalDrift(t *testing.T) {
	if !intervalDrift(5*time.Minute, 15*time.Minute) || intervalDrift(15*time.Minute, 15*time.Minute) {
		t.Fatal("drift is a timer interval other than every_sec")
	}
}

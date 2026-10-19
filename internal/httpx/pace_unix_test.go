//go:build unix

package httpx

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"sync"
	"syscall"
	"testing"
	"time"
)

// TestPaceSpacesRequestsAfterAFreeze: when the whole process stops for a while (the
// machine sleeps), the turns of the waiting requests pass while nothing runs. On waking
// they still go one interval apart, not all at once.
func TestPaceSpacesRequestsAfterAFreeze(t *testing.T) {
	const host, interval = "freeze.test", 100 * time.Millisecond
	SetPace(host, interval)
	t.Cleanup(func() { SetPace(host, 0) })
	var mu sync.Mutex
	var starts []time.Time
	var wg sync.WaitGroup
	for range 5 { // one goes at once; the others wait for turns at 100, 200, 300 and 400ms
		wg.Go(func() {
			if err := Wait(context.Background(), host); err != nil {
				t.Error(err)
			}
			mu.Lock()
			starts = append(starts, time.Now())
			mu.Unlock()
		})
	}
	time.Sleep(10 * time.Millisecond)
	freeze(t, 350*time.Millisecond) // the turns at 100, 200 and 300ms pass
	wg.Wait()
	slices.SortFunc(starts, func(a, b time.Time) int { return a.Compare(b) })
	for i := 1; i < len(starts); i++ {
		if gap := starts[i].Sub(starts[i-1]); gap < interval/2 {
			t.Errorf("requests %d and %d went %v apart, want about %v", i, i+1, gap, interval)
		}
	}
}

// freeze stops this process for d with SIGSTOP; a shell started first resumes it.
func freeze(t *testing.T, d time.Duration) {
	t.Helper()
	pid := os.Getpid()
	resume := exec.Command("sh", "-c", fmt.Sprintf("sleep %.3f; kill -CONT %d", d.Seconds(), pid))
	if err := resume.Start(); err != nil {
		t.Skipf("no shell to resume the process: %v", err)
	}
	if err := syscall.Kill(pid, syscall.SIGSTOP); err != nil {
		t.Fatal(err)
	}
	if err := resume.Wait(); err != nil {
		t.Fatalf("resuming the process: %v", err)
	}
}

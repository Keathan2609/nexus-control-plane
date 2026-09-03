package v1

import (
	"sync"
	"testing"
)

func TestAdmissionRefusesBeyondCapacity(t *testing.T) {
	a := newAdmission(3)

	for i := 0; i < 3; i++ {
		if !a.acquire() {
			t.Fatalf("slot %d should have been available", i)
		}
	}
	if a.acquire() {
		t.Fatal("a fourth request was admitted beyond a capacity of three")
	}
	if got := a.InFlight(); got != 3 {
		t.Fatalf("in-flight is %d, want 3", got)
	}
	if got := a.Rejected(); got != 1 {
		t.Fatalf("rejected is %d, want 1", got)
	}
}

func TestAdmissionReusesReleasedSlots(t *testing.T) {
	a := newAdmission(2)

	if !a.acquire() || !a.acquire() {
		t.Fatal("both slots should have been available")
	}
	if a.acquire() {
		t.Fatal("capacity was exceeded")
	}

	a.release()
	if got := a.InFlight(); got != 1 {
		t.Fatalf("in-flight is %d after a release, want 1", got)
	}
	if !a.acquire() {
		t.Fatal("a released slot was not reusable")
	}
}

// Releasing more often than acquiring must not create capacity out of nothing. A
// double release on some error path would otherwise silently raise the ceiling.
func TestAdmissionIgnoresUnmatchedRelease(t *testing.T) {
	a := newAdmission(1)

	a.release()
	a.release()

	if !a.acquire() {
		t.Fatal("the single slot should still be available")
	}
	if a.acquire() {
		t.Fatal("an unmatched release manufactured extra capacity")
	}
}

// Capacity must hold exactly under concurrent contention, not approximately.
func TestAdmissionIsExactUnderConcurrency(t *testing.T) {
	const capacity = 8
	const callers = 200

	a := newAdmission(capacity)

	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		admitted int
	)
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if a.acquire() {
				mu.Lock()
				admitted++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	if admitted != capacity {
		t.Fatalf("%d callers were admitted against a capacity of %d", admitted, capacity)
	}
	if got := a.Rejected(); got != callers-capacity {
		t.Fatalf("rejected %d, want %d", got, callers-capacity)
	}
}

// A capacity below one would deadlock the deploy path entirely; it is clamped instead.
func TestAdmissionClampsNonsensicalCapacity(t *testing.T) {
	for _, capacity := range []int{0, -1, -100} {
		a := newAdmission(capacity)
		if a.Capacity() != 1 {
			t.Fatalf("capacity %d became %d, want 1", capacity, a.Capacity())
		}
		if !a.acquire() {
			t.Fatalf("capacity %d admitted nothing at all", capacity)
		}
	}
}

package guard

import (
	"testing"
	"time"
)

func TestGuard(t *testing.T) {
	now := time.Unix(0, 0)
	g := New(3, time.Minute, 10*time.Minute)
	g.now = func() time.Time { return now }

	for i := range 2 {
		if g.Fail("a") {
			t.Fatalf("shut out at failure %d of 3", i+1)
		}
	}
	if banned, _ := g.Banned("a"); banned {
		t.Fatal("banned after two failures")
	}
	if !g.Fail("a") {
		t.Fatal("the third failure did not shut the address out")
	}
	if banned, wait := g.Banned("a"); !banned || wait != 10*time.Minute {
		t.Fatalf("Banned = %v, %v; want true, 10m", banned, wait)
	}
	if banned, _ := g.Banned("b"); banned {
		t.Fatal("another address is shut out")
	}
	now = now.Add(10 * time.Minute)
	if banned, _ := g.Banned("a"); banned {
		t.Fatal("still shut out after the ban")
	}

	// Failures spread wider than the window do not add up.
	for i := range 5 {
		if g.Fail("c") {
			t.Fatalf("failure %d, a minute after the one before, shut the address out", i+1)
		}
		now = now.Add(time.Minute)
	}
}

func TestNilGuard(t *testing.T) {
	var g *Guard
	if g.Fail("a") {
		t.Fatal("a nil guard shut an address out")
	}
	if banned, _ := g.Banned("a"); banned {
		t.Fatal("a nil guard reports a ban")
	}
	if New(0, time.Minute, time.Minute) != nil {
		t.Fatal("New(0, ...) should be nil")
	}
}

package guard

import (
	"testing"
	"time"
)

func TestGuard(t *testing.T) {
	now := time.Unix(0, 0)
	g := New(3, time.Minute, 10*time.Minute)
	g.now = func() time.Time { return now }

	if g.Fail("a") || g.Fail("a") {
		t.Fatal("shut out before the third failure")
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
	for range 5 {
		if g.Fail("c") {
			now = now.Add(time.Minute)
			t.Fatal("failures a minute apart shut the address out")
		}
		g.Fail("c")
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

package main

import (
	"testing"

	"github.com/bborn/on/internal/fleet"
	"github.com/bborn/on/internal/inventory"
)

func st(name string, prio, avail int) fleet.Status {
	return fleet.Status{Host: inventory.Host{Name: name, Priority: prio}, Reachable: true, AvailMB: avail}
}

func TestChooseHostPrefersLowerPriorityOverMoreMemory(t *testing.T) {
	h, _, ok := chooseHost([]fleet.Status{st("small", 1, 30000), st("big", 0, 9000)}, 8000, nil)
	if !ok || h.Name != "big" {
		t.Fatalf("got %q ok=%v, want big", h.Name, ok)
	}
}

func TestChooseHostEqualPriorityFallsBackToMostMemory(t *testing.T) {
	h, _, _ := chooseHost([]fleet.Status{st("a", 0, 9000), st("b", 0, 20000)}, 0, nil)
	if h.Name != "b" {
		t.Fatalf("got %q, want b", h.Name)
	}
}

func TestChooseHostSkipsBusyAndShortHosts(t *testing.T) {
	sts := []fleet.Status{st("busy", 0, 90000), st("short", 1, 1000), st("next", 2, 9000)}
	busy := func(h inventory.Host) bool { return h.Name == "busy" }
	h, _, ok := chooseHost(sts, 8000, busy)
	if !ok || h.Name != "next" {
		t.Fatalf("got %q ok=%v, want next", h.Name, ok)
	}
}

func TestChooseHostReportsNoneWhenAllPassedOver(t *testing.T) {
	sts := []fleet.Status{st("busy", 0, 90000), {Host: inventory.Host{Name: "down"}, Reachable: false, AvailMB: 99999}}
	if _, _, ok := chooseHost(sts, 0, func(inventory.Host) bool { return true }); ok {
		t.Fatal("want no host when every candidate is busy or unreachable")
	}
}

func TestLockBusyIsNilWithoutALock(t *testing.T) {
	if lockBusy(inventory.ExecConfig{}) != nil {
		t.Fatal("a project without a lock must never make a host busy")
	}
}

func TestLockBusyAsksTheHostAtItsLockPath(t *testing.T) {
	old := lockHeld
	defer func() { lockHeld = old }()
	var asked string
	lockHeld = func(h inventory.Host, p string) bool { asked = p; return true }
	busy := lockBusy(inventory.ExecConfig{Lock: "myapp"})
	if !busy(inventory.Host{Name: "x", Workdir: "~/projects"}) || asked == "" {
		t.Fatalf("busy not reported or lock path not passed (asked %q)", asked)
	}
}

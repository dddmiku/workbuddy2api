// ═══ 更新日志 ═══
// 2026-09-19：复现免费号独占导致未知号饥饿，并锁定分模型探索、粘性隔离及健康过滤。
package pool

import (
	"fmt"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
)

func explorationPool(t *testing.T) *Pool {
	t.Helper()
	p := New("")
	p.SetRandomSource(func(int64) int64 { return 0 })
	for _, uid := range []string{"free", "unknown-a", "unknown-b", "unknown-c"} {
		p.Add(&auth.Auth{UID: uid})
	}
	p.NoteModelCost("free", "m", 0, 1000)
	return p
}

func TestFreeAccountDoesNotStarveUnknownAccounts(t *testing.T) {
	p := explorationPool(t)
	counts := map[string]int{}
	for i := 0; i < 24; i++ {
		a := p.PickExcludingForRealm(nil, "m", "cn")
		if a == nil {
			t.Fatal("no account selected")
		}
		counts[a.UID]++
		// Real successful free requests keep renewing the observation.
		if a.UID == "free" {
			p.NoteModelCost(a.UID, "m", 0, 1000)
		}
	}
	if counts["free"] != 18 {
		t.Errorf("free account received %d/24 requests; want 18 with bounded exploration", counts["free"])
	}
	for _, uid := range []string{"unknown-a", "unknown-b", "unknown-c"} {
		if counts[uid] != 2 {
			t.Errorf("%s received %d requests; want 2 so healthy unknown accounts cannot starve", uid, counts[uid])
		}
	}
}

func TestExplorationIsIndependentOfStickyAndOtherModelTraffic(t *testing.T) {
	p := explorationPool(t)
	p.NoteModelCost("free", "other", 0, 1000)
	for i := 0; i < 8; i++ {
		for j := 0; j < 7; j++ {
			p.PickByUIDForModel("unknown-a", "m")
			p.PickExcludingForRealm(nil, "other", "cn")
		}
		a := p.PickExcludingForRealm(nil, "m", "cn")
		want := "free"
		if i == 3 {
			want = "unknown-a"
		} else if i == 7 {
			want = "unknown-b"
		}
		if a == nil || a.UID != want {
			t.Fatalf("selection %d = %v, want %s; unrelated traffic changed exploration", i+1, a, want)
		}
	}
}

func TestExplorationRespectsEligibility(t *testing.T) {
	p := explorationPool(t)
	p.SetMaxInFlight(1)
	p.Disable("unknown-a", "fixture")
	p.CooldownSoftForModel("unknown-b", time.Hour, time.Now().Add(time.Hour), "m", "6004")
	p.Add(&auth.Auth{UID: "busy"})
	if !p.Acquire("busy") {
		t.Fatal("acquire fixture")
	}
	defer p.Release("busy")
	p.Add(&auth.Auth{UID: "excluded"})
	p.Add(&auth.Auth{UID: "foreign", Domain: "www.workbuddy.ai"})
	for i := 0; i < 16; i++ {
		a := p.PickExcludingForRealm(map[string]bool{"excluded": true}, "m", "cn")
		want := "free"
		if i%4 == 3 {
			want = "unknown-c"
		}
		if a == nil || a.UID != want {
			t.Fatalf("selection %d = %v, want %s", i, a, want)
		}
	}
}

func TestExplorationLearnsAdditionalFreeAccounts(t *testing.T) {
	p := explorationPool(t)
	seen := map[string]bool{}
	for i := 0; i < 16; i++ {
		a := p.PickExcludingForRealm(nil, "m", "cn")
		if a == nil {
			t.Fatal("no account")
		}
		seen[a.UID] = true
		p.NoteModelCost(a.UID, "m", 0, 1000)
	}
	if len(seen) != 4 {
		t.Fatalf("only %d/4 free accounts discovered: %v", len(seen), seen)
	}
}

func TestExplorationStateIsBounded(t *testing.T) {
	p := explorationPool(t)
	for i := 0; i < maxExplorationScopes+40; i++ {
		model := fmt.Sprintf("model-%d", i)
		p.NoteModelCost("free", model, 0, 1000)
		if p.PickExcludingForRealm(nil, model, "cn") == nil {
			t.Fatal("selection failed")
		}
		if len(p.costExploration) > maxExplorationScopes {
			t.Fatal("exploration state grew beyond its bound")
		}
	}
}

func TestExplorationRealmIsolation(t *testing.T) {
	p := explorationPool(t)
	p.Add(&auth.Auth{UID: "global-free", Domain: "www.workbuddy.ai"})
	p.Add(&auth.Auth{UID: "global-unknown", Domain: "www.workbuddy.ai"})
	p.NoteModelCost("global-free", "m", 0, 1000)
	for i := 0; i < 4; i++ {
		for j := 0; j < 5; j++ {
			p.PickExcludingForRealm(nil, "m", "global")
		}
		a := p.PickExcludingForRealm(nil, "m", "cn")
		want := "free"
		if i == 3 {
			want = "unknown-a"
		}
		if a == nil || a.UID != want {
			t.Fatalf("CN selection %d=%v, want %s", i, a, want)
		}
	}
}

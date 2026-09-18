// ═══ 更新日志 ═══
// 2026-09-18：以多个真实 Pool 共用状态文件复现旧快照覆盖，验证账号/字段增量、显式启停及累计计数不会丢失。
// 2026-09-18：补快照导入不重复累计、未入盘创建意图不能越过删除、旧余额下真实扣费不被少记。
package pool

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
)

func runtimeSharedPools(t *testing.T, disabled bool) (string, *Pool, *Pool) {
	t.Helper()
	file := filepath.Join(t.TempDir(), "state.json")
	seed := New(file)
	seed.Add(&auth.Auth{UID: "account-a"})
	seed.Add(&auth.Auth{UID: "account-b"})
	seed.SetCredits("account-a", 100)
	seed.SetCredits("account-b", 200)
	if disabled {
		seed.Disable("account-a", "operator disabled")
	}
	seed.Close()
	first, second := New(file), New(file)
	t.Cleanup(first.Close)
	t.Cleanup(second.Close)
	return file, first, second
}

func TestRuntimeSnapshotImportDoesNotDuplicateCounters(t *testing.T) {
	for _, latestCount := range []int{5, 9} {
		for _, newSuccess := range []bool{false, true} {
			t.Run(fmt.Sprintf("latest_%d_new_%t", latestCount, newSuccess), func(t *testing.T) {
				file := filepath.Join(t.TempDir(), "state.json")
				seed := New(file)
				seed.Add(&auth.Auth{UID: "account-a"})
				seed.SetCredits("account-a", 100)
				for i := 0; i < 5; i++ {
					seed.NoteSuccess("account-a")
				}
				seed.Close()
				old := New(file)
				t.Cleanup(old.Close)
				if latestCount == 9 {
					other := New(file)
					for i := 0; i < 4; i++ {
						other.NoteSuccess("account-a")
					}
					other.Close()
				}
				raw, err := json.Marshal(snapshot{SavedAt: time.Now().Add(time.Minute), stateFile: stateFile{Accounts: map[string]stateAccount{
					"account-a": {Credits: 100, SuccessCount: 9},
				}}})
				if err != nil {
					t.Fatal(err)
				}
				old.SetStore(&runtimeSnapshotStore{raw: raw})
				old.RestoreFromSnapshot()
				if status, _ := old.Status("account-a"); status.SuccessCount != 9 {
					t.Fatal("snapshot was not adopted")
				}
				want := int64(9)
				if newSuccess {
					old.NoteSuccess("account-a")
					want++
				}
				old.Close()
				if status := runtimeReopenStatus(t, file, "account-a"); status.SuccessCount != want {
					t.Fatalf("imported counters were counted as local events: got=%d want=%d", status.SuccessCount, want)
				}
			})
		}
	}
}

func TestRuntimeUnpersistedCreateCannotUndoDeletion(t *testing.T) {
	for _, recreator := range []string{"deleting_pool", "new_pool", "stale_pool"} {
		t.Run(recreator, func(t *testing.T) {
			file := filepath.Join(t.TempDir(), "state.json")
			first, stale := New(file), New(file)
			t.Cleanup(first.Close)
			t.Cleanup(stale.Close)
			first.Add(&auth.Auth{UID: "account-a"})
			stale.Add(&auth.Auth{UID: "account-a"})
			first.SetCredits("account-a", 100)
			first.Flush()
			first.SyncToDir(nil)
			stale.NoteSuccess("account-a")
			stale.Flush()
			check := New(file)
			_, resurrected := check.Status("account-a")
			check.Close()
			if resurrected {
				t.Fatal("an unpersisted created intent resurrected a deleted account")
			}
			creator := first
			if recreator == "new_pool" {
				creator = New(file)
				t.Cleanup(creator.Close)
			}
			if recreator == "stale_pool" {
				creator = stale
			}
			creator.Add(&auth.Auth{UID: "account-a"})
			creator.SetCredits("account-a", 300)
			creator.Flush()
			if status := runtimeReopenStatus(t, file, "account-a"); status.Credits != 300 {
				t.Fatal("explicit Add after deletion could not recreate the account")
			}
		})
	}
}

func TestRuntimeCreditDeltaUsesActualConsumption(t *testing.T) {
	for _, explicitAssignment := range []bool{false, true} {
		t.Run(fmt.Sprintf("explicit_assignment_%t", explicitAssignment), func(t *testing.T) {
			file, fresh, stale := runtimeSharedPools(t, false)
			fresh.SetCredits("account-a", 1000)
			fresh.Flush()
			if explicitAssignment {
				stale.SetCredits("account-a", 1000)
			}
			stale.NoteModelCost("account-a", "model-a", 150, 1000)
			if status, _ := stale.Status("account-a"); status.Credits < 0 {
				t.Fatal("local balance became negative")
			}
			stale.Close()
			fresh.Close()
			if status := runtimeReopenStatus(t, file, "account-a"); status.Credits != 850 {
				t.Fatalf("actual consumption was clipped or charged twice: credits=%d want=850", status.Credits)
			}
		})
	}
}

func runtimeReopenStatus(t *testing.T, file, uid string) Status {
	t.Helper()
	p := New(file)
	t.Cleanup(p.Close)
	status, ok := p.Status(uid)
	if !ok {
		t.Fatalf("persisted account is missing: %s", uid)
	}
	return status
}

func TestRuntimeSharedPoolsPreserveDifferentAccounts(t *testing.T) {
	file, first, second := runtimeSharedPools(t, false)
	first.SetCredits("account-a", 111)
	second.CooldownSoftForModel("account-b", time.Minute, time.Now().Add(time.Hour), "model-b", "6004 model limit")
	first.Flush()
	second.Flush()
	first.NoteSuccess("account-a")
	first.Close()
	second.NoteSuccess("account-b")
	second.Close()
	a := runtimeReopenStatus(t, file, "account-a")
	b := runtimeReopenStatus(t, file, "account-b")
	if a.Credits != 111 || a.SuccessCount != 1 || b.SuccessCount != 1 || len(b.RateLimitedModels) != 1 {
		t.Fatalf("shared state lost an account update: a_credits=%d a_success=%d b_success=%d b_model_limits=%d", a.Credits, a.SuccessCount, b.SuccessCount, len(b.RateLimitedModels))
	}
}

func TestRuntimeSharedPoolsPreserveDifferentFields(t *testing.T) {
	file, first, second := runtimeSharedPools(t, false)
	first.Disable("account-a", "operator disabled")
	second.SetCredits("account-a", 250)
	first.Flush()
	second.Flush()
	first.NoteSuccess("account-a")
	first.Close()
	second.Close()
	status := runtimeReopenStatus(t, file, "account-a")
	if !status.Disabled || status.Credits != 250 || status.SuccessCount != 1 {
		t.Fatalf("stale account fields replaced newer state: disabled=%t credits=%d success=%d", status.Disabled, status.Credits, status.SuccessCount)
	}
}

func TestRuntimeSharedPoolsKeepExplicitAvailabilityIntent(t *testing.T) {
	for _, explicitDisable := range []bool{false, true} {
		t.Run(fmt.Sprintf("explicit_disable_%t", explicitDisable), func(t *testing.T) {
			file, old, current := runtimeSharedPools(t, true)
			current.ReviveDisabled("account-a")
			current.Flush()
			if explicitDisable {
				old.Disable("account-a", "operator disabled")
			} else {
				old.NoteSuccess("account-a")
			}
			old.Close()
			current.Close()
			status := runtimeReopenStatus(t, file, "account-a")
			if status.Disabled != explicitDisable {
				t.Fatalf("availability intent was lost: disabled=%t want=%t", status.Disabled, explicitDisable)
			}
		})
	}
}

func TestRuntimeSharedPoolsMergeModelLimits(t *testing.T) {
	file, first, second := runtimeSharedPools(t, false)
	first.CooldownSoftForModel("account-a", time.Minute, time.Now().Add(time.Hour), "model-a", "6004 model limit")
	second.CooldownSoftForModel("account-a", time.Minute, time.Now().Add(time.Hour), "model-b", "6004 model limit")
	first.Flush()
	second.Flush()
	first.NoteSuccess("account-a")
	first.Close()
	second.Close()
	status := runtimeReopenStatus(t, file, "account-a")
	if len(status.RateLimitedModels) != 2 {
		t.Fatalf("independent model limits overwrote each other: limits=%d want=2", len(status.RateLimitedModels))
	}
}

func TestRuntimeSharedPoolsAccumulateCounters(t *testing.T) {
	file, first, second := runtimeSharedPools(t, false)
	first.NoteSuccess("account-a")
	first.NoteSuccess("account-a")
	second.NoteSuccess("account-a")
	second.NoteSuccess("account-a")
	second.NoteSuccess("account-a")
	first.Flush()
	second.Flush()
	first.NoteSuccess("account-a")
	first.Close()
	second.Close()
	status := runtimeReopenStatus(t, file, "account-a")
	if status.SuccessCount != 6 {
		t.Fatalf("concurrent success deltas were lost: success=%d want=6", status.SuccessCount)
	}
}

func TestRuntimeSharedPoolsKeepExplicitCreditAssignment(t *testing.T) {
	file, first, second := runtimeSharedPools(t, false)
	first.SetCredits("account-a", 7)
	first.Flush()
	second.SetCredits("account-a", 100)
	second.Flush()
	first.NoteSuccess("account-a")
	first.Close()
	second.Close()
	if status := runtimeReopenStatus(t, file, "account-a"); status.Credits != 100 {
		t.Fatalf("an explicit assignment equal to the local baseline was lost: credits=%d", status.Credits)
	}
}

func TestRuntimeSharedPoolsAccumulateCreditDeductions(t *testing.T) {
	file, first, second := runtimeSharedPools(t, false)
	first.NoteModelCost("account-a", "model-a", 3, 1000)
	second.NoteModelCost("account-a", "model-a", 4, 1000)
	first.Close()
	second.Close()
	if status := runtimeReopenStatus(t, file, "account-a"); status.Credits != 93 {
		t.Fatalf("independent credit deductions did not accumulate: credits=%d want=93", status.Credits)
	}
}

func TestRuntimeSharedPoolsDoNotResurrectRemovedAccounts(t *testing.T) {
	file, first, second := runtimeSharedPools(t, false)
	first.SyncToDir([]*auth.Auth{{UID: "account-b"}})
	second.NoteSuccess("account-b")
	second.Close()
	first.Close()
	reopened := New(file)
	t.Cleanup(reopened.Close)
	if _, exists := reopened.Status("account-a"); exists {
		t.Fatal("a stale snapshot resurrected an explicitly removed account")
	}
}

func TestRuntimeSharedPoolsCoordinateConcurrentWrites(t *testing.T) {
	file := filepath.Join(t.TempDir(), "state.json")
	seed := New(file)
	for i := 0; i < 8; i++ {
		uid := fmt.Sprintf("account-%d", i)
		seed.Add(&auth.Auth{UID: uid})
		seed.SetCredits(uid, 0)
	}
	seed.Close()
	pools := make([]*Pool, 8)
	for i := range pools {
		pools[i] = New(file)
	}
	start := make(chan struct{})
	var workers sync.WaitGroup
	for i, p := range pools {
		workers.Add(1)
		go func(i int, p *Pool) {
			defer workers.Done()
			<-start
			p.NoteSuccess(fmt.Sprintf("account-%d", i))
			p.Close()
		}(i, p)
	}
	close(start)
	workers.Wait()
	for i := range pools {
		status := runtimeReopenStatus(t, file, fmt.Sprintf("account-%d", i))
		if status.SuccessCount != 1 {
			t.Fatalf("concurrent file writes lost account-%d: success=%d", i, status.SuccessCount)
		}
	}
}

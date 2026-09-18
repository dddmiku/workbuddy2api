// ═══ 更新日志 ═══
// 2026-09-18：复现本地状态缺失时忽略远端快照及写盘失败后不再重试，保护重启后的账号状态。
package pool

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
)

type runtimeSnapshotStore struct{ raw []byte }

func (s *runtimeSnapshotStore) SaveState(raw []byte)      { s.raw = append([]byte(nil), raw...) }
func (s *runtimeSnapshotStore) LoadState() ([]byte, bool) { return s.raw, len(s.raw) != 0 }

func TestRuntimeRestoreSnapshotWithoutLocalState(t *testing.T) {
	raw, err := json.Marshal(snapshot{SavedAt: time.Now(), stateFile: stateFile{Accounts: map[string]stateAccount{
		"account-runtime": {Credits: 321, Disabled: true, Reason: "operator disabled"},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	p := New(filepath.Join(t.TempDir(), "state.json"))
	t.Cleanup(p.Close)
	p.SetStore(&runtimeSnapshotStore{raw: raw})
	p.RestoreFromSnapshot()
	p.Add(&auth.Auth{UID: "account-runtime"})
	status, ok := p.Status("account-runtime")
	if !ok || status.Credits != 321 || !status.Disabled {
		t.Fatalf("missing local state discarded remote account state: found=%t credits=%d disabled=%t", ok, status.Credits, status.Disabled)
	}
}

func TestRuntimeFlushRetriesAfterFilesystemRecovers(t *testing.T) {
	file := filepath.Join(t.TempDir(), "state.json")
	p := New(file)
	t.Cleanup(p.Close)
	p.Add(&auth.Auth{UID: "account-runtime"})
	if err := os.Mkdir(file, 0o700); err != nil {
		t.Fatal(err)
	}
	p.NoteSuccess("account-runtime")
	p.Flush()
	if err := os.Remove(file); err != nil {
		t.Fatal(err)
	}
	p.Flush()
	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("pending state was not retried after the filesystem recovered: %v", err)
	}
	var saved stateFile
	if err := json.Unmarshal(raw, &saved); err != nil {
		t.Fatal(err)
	}
	if saved.Accounts["account-runtime"].SuccessCount != 1 {
		t.Fatal("retry did not preserve the pending success counter")
	}
}

func TestRuntimeFallbackDoesNotBypassModelCooldown(t *testing.T) {
	for _, source := range []string{"rate_limit", "unavailable_model"} {
		t.Run(source, func(t *testing.T) {
			p := New("")
			p.Add(&auth.Auth{UID: "account-runtime"})
			if source == "rate_limit" {
				p.CooldownSoftForModel("account-runtime", time.Minute, time.Now().Add(time.Hour), "blocked-model", "6004 model limit")
			} else {
				p.BlockModelBackoff("account-runtime", "blocked-model", "11102 model unavailable")
			}
			p.SetBreaker(1, time.Minute, time.Minute)
			p.NoteError("account-runtime")
			if got := p.PickExcludingForRealm(nil, "blocked-model", "cn"); got != nil {
				t.Fatal("all-cooled fallback selected an account whose requested model is still blocked")
			}
			if got := p.PickExcludingForRealm(nil, "other-model", "cn"); got == nil {
				t.Fatal("model guard disabled the existing breaker fallback for unrelated models")
			}
		})
	}
}

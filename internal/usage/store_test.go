// ═══ 更新日志 ═══
// 2026-09-17：锁定用量账本的累计、按模型拆分、原子落盘与重启恢复语义。
package usage

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestRecordAccumulatesPerKeyAndPerModel(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "usage.json")
	store, err := Open(path, time.Hour)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer store.Close()

	at := time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC)
	store.Record("key_a", "团队 A", "wb2a_ab…cd", "cn:deepseek-v4.1-flash", 100, 40, 0.5, true, at)
	store.Record("key_a", "团队 A", "wb2a_ab…cd", "cn:glm-5.2", 10, 5, 0, false, at.Add(time.Minute))
	store.Record("key_b", "团队 B", "wb2a_ef…gh", "cn:deepseek-v4.1-flash", 7, 3, 0, false, at)
	store.Record("", "", "", "cn:deepseek-v4.1-flash", 1, 1, 0, false, at)

	snapshot := store.Snapshot()
	if snapshot.Totals.Requests != 4 {
		t.Fatalf("total requests = %d want 4", snapshot.Totals.Requests)
	}
	if snapshot.Totals.PromptTokens != 118 || snapshot.Totals.CompletionTokens != 49 {
		t.Fatalf("totals = %+v", snapshot.Totals)
	}
	if snapshot.Totals.TotalTokens != 167 {
		t.Fatalf("total tokens = %d want 167", snapshot.Totals.TotalTokens)
	}
	if snapshot.Totals.Credit != 0.5 {
		t.Fatalf("credit = %v want 0.5 (only explicit credit counted)", snapshot.Totals.Credit)
	}
	if len(snapshot.Keys) != 3 {
		t.Fatalf("keys = %d want 3 (key_a, key_b, legacy)", len(snapshot.Keys))
	}
	if snapshot.Keys[0].KeyID != "key_a" || snapshot.Keys[0].Totals.TotalTokens != 155 {
		t.Fatalf("first key = %+v", snapshot.Keys[0])
	}
	if len(snapshot.Keys[0].Models) != 2 {
		t.Fatalf("key_a models = %d want 2", len(snapshot.Keys[0].Models))
	}
	if snapshot.Keys[0].Models[0].Model != "cn:deepseek-v4.1-flash" {
		t.Fatalf("models not sorted by tokens: %+v", snapshot.Keys[0].Models)
	}
	last := snapshot.Keys[2]
	if last.KeyID != "legacy" || last.Totals.Requests != 1 {
		t.Fatalf("legacy bucket = %+v", last)
	}
}

func TestFlushAndReload(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "usage.json")
	store, err := Open(path, time.Hour)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	at := time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC)
	store.Record("key_a", "团队 A", "wb2a_ab…cd", "cn:deepseek-v4.1-flash", 100, 40, 0, false, at)
	if err := store.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var doc document
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if doc.Version != Version || doc.Totals.TotalTokens != 140 {
		t.Fatalf("persisted doc = %+v", doc)
	}

	reopened, err := Open(path, time.Hour)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()
	snapshot := reopened.Snapshot()
	if snapshot.Totals.TotalTokens != 140 || len(snapshot.Keys) != 1 {
		t.Fatalf("reloaded snapshot = %+v", snapshot)
	}
	if snapshot.Since.IsZero() {
		t.Fatalf("since must be preserved across restarts")
	}
}

func TestRecordAfterCloseIsIgnored(t *testing.T) {
	dir := t.TempDir()
	store, err := Open(filepath.Join(dir, "usage.json"), time.Hour)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	store.Record("key_a", "团队 A", "wb2a_ab…cd", "cn:deepseek-v4.1-flash", 10, 10, 0, false, time.Now())
	if got := store.Snapshot().Totals.Requests; got != 0 {
		t.Fatalf("requests after close = %d want 0", got)
	}
}

func TestResetClearsCounters(t *testing.T) {
	dir := t.TempDir()
	store, err := Open(filepath.Join(dir, "usage.json"), time.Hour)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer store.Close()
	store.Record("key_a", "团队 A", "wb2a_ab…cd", "cn:deepseek-v4.1-flash", 5, 5, 0, false, time.Now())
	if err := store.Reset(time.Now()); err != nil {
		t.Fatalf("reset: %v", err)
	}
	snapshot := store.Snapshot()
	if snapshot.Totals.Requests != 0 || len(snapshot.Keys) != 0 {
		t.Fatalf("reset snapshot = %+v", snapshot)
	}
}

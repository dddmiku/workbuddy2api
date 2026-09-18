// ═══ 更新日志 ═══
// 2026-09-17：锁定用量账本的累计、按模型拆分、原子落盘与重启恢复语义。
// 2026-09-17：锁定热更新期间新旧进程共用账本时的合并语义（取大不丢不重）。
// 2026-09-17：锁定跨进程可见性：另一个进程落盘的记录要立刻出现在本进程快照里，
//
//	且不会因为"把它算成自己的增量"而重复计数。
//
// 2026-09-18：锁定缓存命中输入维度能穿过「盘上 + 本方增量」合并与重开恢复（曾因子段枚举漏写而丢）。
// 2026-09-18：锁定按天分桶：总量与按密钥都要有当天数据，且穿过落盘/合并/重开。
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
	store.Record("key_a", "团队 A", "wb2a_ab…cd", "cn:deepseek-v4.1-flash", 100, 40, 0, 0.5, true, at)
	store.Record("key_a", "团队 A", "wb2a_ab…cd", "cn:glm-5.2", 10, 5, 0, 0, false, at.Add(time.Minute))
	store.Record("key_b", "团队 B", "wb2a_ef…gh", "cn:deepseek-v4.1-flash", 7, 3, 0, 0, false, at)
	store.Record("", "", "", "cn:deepseek-v4.1-flash", 1, 1, 0, 0, false, at)

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
	store.Record("key_a", "团队 A", "wb2a_ab…cd", "cn:deepseek-v4.1-flash", 100, 40, 0, 0, false, at)
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

// TestCachedTokensSurviveMergeAndReload 缓存命中维度必须和其它维度一样，穿过快照合并、
// 落盘与重开恢复。快照合并按字段枚举，漏写新字段会让页面上永远显示 0。
func TestCachedTokensSurviveMergeAndReload(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "usage.json")
	store, err := Open(path, time.Hour)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	at := time.Date(2026, 9, 18, 3, 0, 0, 0, time.UTC)
	// 5000 输入里 4096 命中缓存：这正是思考模式每轮重发整段上下文的典型形状。
	store.Record("key_a", "团队 A", "wb2a_ab…cd", "cn:deepseek-v4.1-flash", 5000, 120, 4096, 0, false, at)

	snapshot := store.Snapshot()
	if snapshot.Totals.CachedTokens != 4096 {
		t.Fatalf("snapshot cached = %d want 4096（合并路径丢了缓存字段）", snapshot.Totals.CachedTokens)
	}
	if len(snapshot.Keys) != 1 || snapshot.Keys[0].Totals.CachedTokens != 4096 ||
		len(snapshot.Keys[0].Models) != 1 || snapshot.Keys[0].Models[0].Totals.CachedTokens != 4096 {
		t.Fatalf("per-key/per-model cached lost: %+v", snapshot.Keys)
	}

	if err := store.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	reopened, err := Open(path, time.Hour)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()
	restored := reopened.Snapshot()
	if restored.Totals.CachedTokens != 4096 || restored.Totals.PromptTokens != 5000 {
		t.Fatalf("reloaded cached = %d prompt = %d want 4096 / 5000",
			restored.Totals.CachedTokens, restored.Totals.PromptTokens)
	}
}

// TestDayBucketsTrackLocalCalendarDay 天桶按服务端本地日历日切分：
// 同一天的两笔要落到同一个桶，跨零点要分开，且按密钥维度也要能对上。
func TestDayBucketsTrackLocalCalendarDay(t *testing.T) {
	dir := t.TempDir()
	store, err := Open(filepath.Join(dir, "usage.json"), time.Hour)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer store.Close()

	local := time.FixedZone("CST", 8*3600)
	day1 := time.Date(2026, 9, 18, 1, 0, 0, 0, local)    // 本地 09-18 01:00
	day1b := time.Date(2026, 9, 18, 23, 30, 0, 0, local) // 本地 09-18 23:30（同一日历日）
	day2 := time.Date(2026, 9, 19, 0, 30, 0, 0, local)   // 本地 09-19 00:30（跨零点）
	store.Record("key_a", "团队 A", "wb2a_ab…cd", "cn:deepseek-v4.1-flash", 100, 10, 80, 0, false, day1)
	store.Record("key_a", "团队 A", "wb2a_ab…cd", "cn:deepseek-v4.1-flash", 200, 20, 160, 0, false, day1b)
	store.Record("key_b", "团队 B", "wb2a_ef…gh", "cn:glm-5.2", 50, 5, 0, 0, false, day2)

	snapshot := store.Snapshot()
	byDay := map[string]Totals{}
	for _, day := range snapshot.Days {
		byDay[day.Day] = day.Totals
	}
	// 天桶用 time.Local 归日：测试里传入的是 CST 时刻，容器/CI 的 time.Local 未必是 CST，
	// 因此这里按「同一本地日历日的两笔必须合并、跨日的必须分开」来断言，而不是写死日期串。
	if len(snapshot.Days) != 2 {
		t.Fatalf("天桶数 = %d want 2（本地同一天合并、跨日分开）: %+v", len(snapshot.Days), snapshot.Days)
	}
	total := Totals{}
	for _, day := range snapshot.Days {
		total = addTotals(total, day.Totals)
		if day.Totals.Requests != 2 && day.Totals.Requests != 1 {
			t.Fatalf("单日请求数异常: %+v", day.Totals)
		}
	}
	if total.Requests != 3 || total.PromptTokens != 350 || total.CachedTokens != 240 {
		t.Fatalf("天桶合计与总数不一致: %+v", total)
	}
	// 最新的一天排最前
	if snapshot.Days[0].Day <= snapshot.Days[1].Day {
		t.Fatalf("天桶应按日期倒序: %v", []string{snapshot.Days[0].Day, snapshot.Days[1].Day})
	}
	// 按密钥维度也要有当天数据
	for _, key := range snapshot.Keys {
		if len(key.Days) == 0 {
			t.Fatalf("密钥 %s 缺天桶明细", key.KeyID)
		}
	}
}

// TestDayBucketsSurviveMergeAndReload 天桶必须穿过「盘上 + 本方增量」合并与重开恢复，
// 且新维度漏进子段枚举时本用例会失败（缓存维度就踩过这个坑）。
func TestDayBucketsSurviveMergeAndReload(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "usage.json")
	store, err := Open(path, time.Hour)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	at := time.Now()
	store.Record("key_a", "团队 A", "wb2a_ab…cd", "cn:deepseek-v4.1-flash", 500, 40, 320, 0, false, at)
	snapshot := store.Snapshot()
	if len(snapshot.Days) != 1 {
		t.Fatalf("合并路径丢了天桶: %+v", snapshot.Days)
	}
	if snapshot.Days[0].Totals.PromptTokens != 500 || snapshot.Days[0].Totals.CachedTokens != 320 {
		t.Fatalf("天桶数值不对: %+v", snapshot.Days[0].Totals)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	reopened, err := Open(path, time.Hour)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()
	restored := reopened.Snapshot()
	if len(restored.Days) != 1 || restored.Days[0].Totals.PromptTokens != 500 {
		t.Fatalf("重开后丢天桶: %+v", restored.Days)
	}
}

// TestDayKeyUsesLocalCalendarDay 天键必须是本地日历日（容器 TZ 与用户一致），
// 用 UTC 归日会把本地 00:00–08:00 的请求算到前一天。
func TestDayKeyUsesLocalCalendarDay(t *testing.T) {
	local := time.FixedZone("CST", 8*3600)
	moment := time.Date(2026, 9, 18, 0, 30, 0, 0, local)
	got := dayKeyOf(moment)
	want := moment.In(time.Local).Format(dayKeyLayout)
	if got != want {
		t.Fatalf("dayKeyOf = %q want %q（本地日历日）", got, want)
	}
	if got == moment.UTC().Format(dayKeyLayout) && moment.In(time.Local).Day() != moment.UTC().Day() {
		t.Fatalf("dayKeyOf 用了 UTC 归日: %q", got)
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
	store.Record("key_a", "团队 A", "wb2a_ab…cd", "cn:deepseek-v4.1-flash", 10, 10, 0, 0, false, time.Now())
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
	store.Record("key_a", "团队 A", "wb2a_ab…cd", "cn:deepseek-v4.1-flash", 5, 5, 0, 0, false, time.Now())
	if err := store.Reset(time.Now()); err != nil {
		t.Fatalf("reset: %v", err)
	}
	snapshot := store.Snapshot()
	if snapshot.Totals.Requests != 0 || len(snapshot.Keys) != 0 {
		t.Fatalf("reset snapshot = %+v", snapshot)
	}
}

// TestConcurrentStoresMergeInsteadOfOverwrite 复现热更新窗口：
// 新旧两个进程各自记录，后落盘的一方不能覆盖对方刚写的记录。
func TestConcurrentStoresMergeInsteadOfOverwrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "usage.json")
	oldProcess, err := Open(path, time.Hour)
	if err != nil {
		t.Fatalf("open old: %v", err)
	}
	defer oldProcess.Close()
	newProcess, err := Open(path, time.Hour)
	if err != nil {
		t.Fatalf("open new: %v", err)
	}
	defer newProcess.Close()

	at := time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC)
	oldProcess.Record("key_a", "团队 A", "wb2a_ab…cd", "cn:deepseek-v4.1-flash", 100, 40, 0, 1.5, true, at)
	if err := oldProcess.Flush(); err != nil {
		t.Fatalf("old flush: %v", err)
	}
	// 新进程在旧进程落盘之后才写：磁盘上已经有 key_a 的 140 token。
	newProcess.Record("key_b", "团队 B", "wb2a_ef…gh", "global:deepseek-v4.1-flash", 7, 3, 0, 0, false, at.Add(time.Minute))
	if err := newProcess.Flush(); err != nil {
		t.Fatalf("new flush: %v", err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var doc document
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if doc.Totals.Requests != 2 || doc.Totals.TotalTokens != 150 {
		t.Fatalf("merged totals = %+v want requests=2 tokens=150", doc.Totals)
	}
	if doc.Totals.Credit != 1.5 {
		t.Fatalf("merged credit = %v want 1.5", doc.Totals.Credit)
	}
	if len(doc.Keys) != 2 || doc.Keys["key_a"] == nil || doc.Keys["key_b"] == nil {
		t.Fatalf("merged keys = %+v want both key_a and key_b", doc.Keys)
	}
	if doc.Keys["key_a"].Totals.TotalTokens != 140 {
		t.Fatalf("key_a totals lost: %+v", doc.Keys["key_a"].Totals)
	}
	// 合并只保留较新的更新时间与较早的起始时间。
	if doc.UpdatedAt.Before(at.Add(time.Minute)) {
		t.Fatalf("updated_at = %v want the later write", doc.UpdatedAt)
	}

	// 新进程内存里也要看到合并结果：再落一次盘仍然是 2 请求 150 token。
	if err := newProcess.Flush(); err != nil {
		t.Fatalf("second flush: %v", err)
	}
	reopened, err := Open(path, time.Hour)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()
	snapshot := reopened.Snapshot()
	if snapshot.Totals.Requests != 2 || snapshot.Totals.TotalTokens != 150 {
		t.Fatalf("reloaded totals = %+v want requests=2 tokens=150", snapshot.Totals)
	}
}

// TestSnapshotSeesOtherProcessRecords 热更新窗口里旧进程的收尾记录必须立刻可见。
func TestSnapshotSeesOtherProcessRecords(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "usage.json")
	idle, err := Open(path, time.Hour)
	if err != nil {
		t.Fatalf("open idle: %v", err)
	}
	defer idle.Close()

	// 另一个进程（交接窗口里的旧实例）记一笔并落盘；本进程没有任何流量。
	other, err := Open(path, time.Hour)
	if err != nil {
		t.Fatalf("open other: %v", err)
	}
	at := time.Date(2026, 9, 17, 16, 25, 0, 0, time.UTC)
	other.Record("key_a", "团队 A", "wb2a_ab…cd", "cn:deepseek-v4.1-flash", 308, 7948, 0, 0, false, at)
	if err := other.Flush(); err != nil {
		t.Fatalf("other flush: %v", err)
	}
	if err := other.Close(); err != nil {
		t.Fatalf("other close: %v", err)
	}

	snapshot := idle.Snapshot()
	if snapshot.Totals.Requests != 1 || snapshot.Totals.TotalTokens != 8256 {
		t.Fatalf("idle process snapshot = %+v want 1 request / 8256 tokens", snapshot.Totals)
	}
	if len(snapshot.Keys) != 1 || snapshot.Keys[0].KeyID != "key_a" {
		t.Fatalf("idle process keys = %+v", snapshot.Keys)
	}

	// 本进程再记一笔并落盘：盘上应是两笔之和，且不会把对方那笔重复计入。
	idle.Record("key_b", "团队 B", "wb2a_ef…gh", "cn:deepseek-v4.1-flash", 10, 20, 0, 0, false, at.Add(time.Minute))
	if err := idle.Flush(); err != nil {
		t.Fatalf("idle flush: %v", err)
	}
	after := idle.Snapshot()
	if after.Totals.Requests != 2 || after.Totals.TotalTokens != 8286 {
		t.Fatalf("after own write = %+v want 2 requests / 8286 tokens", after.Totals)
	}
	if len(after.Keys) != 2 {
		t.Fatalf("after own write keys = %+v want both", after.Keys)
	}

	// 再落一次盘仍是同样数字（基线与增量都对得上，不会滚雪球）。
	if err := idle.Flush(); err != nil {
		t.Fatalf("second flush: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var doc document
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if doc.Totals.Requests != 2 || doc.Totals.TotalTokens != 8286 {
		t.Fatalf("on-disk totals = %+v want 2 requests / 8286 tokens", doc.Totals)
	}
}

// TestMergeFromDiskSurvivesBrokenLedger 账本被外部写坏时不能阻断落盘。
func TestMergeFromDiskSurvivesBrokenLedger(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "usage.json")
	store, err := Open(path, time.Hour)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer store.Close()
	store.Record("key_a", "团队 A", "wb2a_ab…cd", "cn:deepseek-v4.1-flash", 5, 5, 0, 0, false, time.Now())
	// 另一个进程写了一半就被杀掉：盘上是半截 JSON。
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatalf("seed broken ledger: %v", err)
	}
	if err := store.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var doc document
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("ledger must be rewritten as valid json: %v", err)
	}
	if doc.Totals.TotalTokens != 10 {
		t.Fatalf("totals = %+v want 10 tokens", doc.Totals)
	}
}

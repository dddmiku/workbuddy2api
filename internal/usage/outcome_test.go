// ═══ 更新日志 ═══
// 2026-09-19：验证失败/未上报请求计数跨维度、并发实例、Flush/Reset/Close及旧账本兼容。
package usage

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func assertOutcomeDimensions(t *testing.T, snapshot Snapshot, want Totals) {
	t.Helper()
	if snapshot.Totals != want {
		t.Fatalf("total=%+v want=%+v", snapshot.Totals, want)
	}
	if len(snapshot.Keys) != 1 || len(snapshot.Keys[0].Models) != 1 || len(snapshot.Days) != 1 || len(snapshot.Keys[0].Days) != 1 {
		t.Fatalf("missing outcome dimension: %+v", snapshot)
	}
	for _, got := range []Totals{snapshot.Keys[0].Totals, snapshot.Keys[0].Models[0].Totals, snapshot.Days[0].Totals, snapshot.Keys[0].Days[0].Totals} {
		if got != want {
			t.Fatalf("dimension=%+v want=%+v", got, want)
		}
	}
}

func TestOutcomeConcurrentStoresPersistEveryDimension(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.json")
	a, err := Open(path, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	b, err := Open(path, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Close(); _ = b.Close() })
	start := make(chan struct{})
	results := make(chan error, 2)
	at := time.Now()
	for index, store := range []*Store{a, b} {
		go func(index int, store *Store) {
			<-start
			for round := 0; round < 20; round++ {
				completion := 2
				if index == 1 {
					completion = -1
				}
				store.RecordOutcome("fixture-key", "fixture", "masked", "model", 10, completion, 5, 0.5, true, Outcome{Failed: index == 0}, at)
				if err := store.Flush(); err != nil {
					results <- err
					return
				}
			}
			results <- nil
		}(index, store)
	}
	close(start)
	for range 2 {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	want := Totals{Requests: 40, FailedRequests: 20, UnreportedRequests: 20, PromptTokens: 400, CompletionTokens: 40, CachedTokens: 200, TotalTokens: 440, Credit: 20}
	assertOutcomeDimensions(t, a.Snapshot(), want)
	assertOutcomeDimensions(t, b.Snapshot(), want)
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(path, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	assertOutcomeDimensions(t, reopened.Snapshot(), want)
}

func TestOutcomeRecordDuringFlushAndConcurrentClose(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.json")
	store, err := Open(path, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Now()
	store.Record("fixture-key", "fixture", "masked", "model", 10, 2, 0, 0, false, at)
	original := store.persist
	var once sync.Once
	store.persist = func(doc document) error {
		once.Do(func() {
			store.RecordOutcome("fixture-key", "fixture", "masked", "model", 7, -1, -1, 0.5, true, Outcome{Failed: true}, at)
		})
		return original(doc)
	}
	if err := store.Flush(); err != nil {
		t.Fatal(err)
	}
	store.persist = original
	want := Totals{Requests: 2, FailedRequests: 1, UnreportedRequests: 1, PromptTokens: 17, CompletionTokens: 2, TotalTokens: 19, Credit: 0.5}
	assertOutcomeDimensions(t, store.Snapshot(), want)
	results := make(chan error, 2)
	for range 2 {
		go func() { results <- store.Close() }()
	}
	for range 2 {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	reopened, err := Open(path, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	assertOutcomeDimensions(t, reopened.Snapshot(), want)
}

func TestOutcomeResetAndLegacyCompatibility(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.json")
	legacy := []byte(`{"version":1,"totals":{"requests":9,"prompt_tokens":100,"completion_tokens":20,"total_tokens":120},"keys":{}}`)
	if err := os.WriteFile(path, legacy, 0600); err != nil {
		t.Fatal(err)
	}
	store, err := Open(path, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if got := store.Snapshot().Totals; got.Requests != 9 || got.FailedRequests != 0 || got.UnreportedRequests != 0 {
		t.Fatalf("invented historical outcomes: %+v", got)
	}
	store.RecordOutcome("fixture-key", "fixture", "masked", "model", 10, -1, -1, 0, false, Outcome{Failed: true}, time.Now())
	if err := store.Flush(); err != nil {
		t.Fatal(err)
	}
	if err := store.Reset(time.Now()); err != nil {
		t.Fatal(err)
	}
	if got := store.Snapshot(); got.Totals != (Totals{}) || len(got.Keys) != 0 || len(got.Days) != 0 {
		t.Fatalf("reset kept outcome counters: %+v", got)
	}
	store.RecordOutcome("fixture-key", "fixture", "masked", "model", 0, 0, 0, 0, true, Outcome{Failed: true}, time.Now())
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(path, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	assertOutcomeDimensions(t, reopened.Snapshot(), Totals{Requests: 1, FailedRequests: 1})
}

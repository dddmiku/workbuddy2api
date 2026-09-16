// ═══ 更新日志 ═══
// 2026-09-16：验证凭据快照不复制锁/不混代、旧刷新不覆盖新凭据，以及并发原子落盘的完整性与失败清理。
package auth

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestCredentialIntegritySnapshotIsIndependent(t *testing.T) {
	a, err := Parse([]byte(`{"accessToken":"old-access","refreshToken":"old-refresh","expiresAt":123,"domain":"copilot.tencent.com","realm":"global","uid":"u","enterpriseId":"e","nickname":"n","device_token":"device"}`))
	if err != nil {
		t.Fatal(err)
	}
	a.FilePath = filepath.Join(t.TempDir(), "auth.json")
	snapshot := a.Snapshot()
	a.Lock()
	a.AccessToken = "new-access"
	a.RefreshToken = "new-refresh"
	a.Unlock()
	if snapshot.AccessToken != "old-access" || snapshot.RefreshToken != "old-refresh" || snapshot.RealmStored() != "global" || snapshot.FilePath != a.FilePath || snapshot.DeviceToken != "device" || snapshot.UID != "u" || snapshot.EnterpriseID != "e" || snapshot.Nickname != "n" || snapshot.ExpiresAt != 123 {
		t.Fatal("snapshot lost fields or changed with its source")
	}
	a.Lock()
	done := make(chan struct{})
	go func() { _ = snapshot.Realm(); _ = snapshot.NeedsRefresh(time.Minute); close(done) }()
	select {
	case <-done:
		a.Unlock()
	case <-time.After(time.Second):
		a.Unlock()
		t.Fatal("snapshot shares or copied its source credential lock")
	}
}

func TestCredentialIntegrityStaleRefreshCannotOverwriteNewToken(t *testing.T) {
	a := &Auth{AccessToken: "old", RefreshToken: "refresh", Domain: "domain", ExpiresAt: 123}
	started, release := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- a.RefreshOnce(func(*Auth) (TokenUpdate, error) {
			close(started)
			<-release
			return TokenUpdate{AccessToken: "stale-response", RefreshToken: "stale-refresh", Domain: "stale-domain", ExpiresAt: 456}, nil
		})
	}()
	<-started
	a.Lock()
	a.AccessToken = "newer-external-token"
	a.Unlock()
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	got := a.Snapshot()
	if got.AccessToken != "newer-external-token" || got.RefreshToken != "refresh" || got.Domain != "domain" || got.ExpiresAt != 123 {
		t.Fatal("old refresh overwrote newer credential state")
	}
}

func TestCredentialIntegrityConcurrentSnapshotsAndAtomicSave(t *testing.T) {
	dir := t.TempDir()
	a := &Auth{UID: "concurrent", AccessToken: "access-0", RefreshToken: "refresh-0", Domain: "domain-0", ExpiresAt: 1000, FilePath: filepath.Join(dir, "workbuddy-concurrent.json")}
	if err := a.SaveAtomic(); err != nil {
		t.Fatal(err)
	}
	check := func(value *Auth) {
		generation := strings.TrimPrefix(value.AccessToken, "access-")
		if value.RefreshToken != "refresh-"+generation || value.Domain != "domain-"+generation {
			t.Errorf("snapshot or file contained mixed credential generations")
		}
	}
	start := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(3)
	go func() {
		defer wg.Done()
		<-start
		for i := 1; i <= 200; i++ {
			generation := i
			if err := a.RefreshOnce(func(*Auth) (TokenUpdate, error) {
				return TokenUpdate{AccessToken: fmt.Sprintf("access-%d", generation), RefreshToken: fmt.Sprintf("refresh-%d", generation), Domain: fmt.Sprintf("domain-%d", generation), ExpiresAt: int64(1000 + generation)}, nil
			}); err != nil {
				t.Error(err)
				return
			}
		}
	}()
	go func() {
		defer wg.Done()
		<-start
		for i := 0; i < 1000; i++ {
			check(a.Snapshot())
			_ = a.NeedsRefresh(time.Minute)
			_ = a.RealmStored()
			_ = a.Realm()
		}
	}()
	go func() {
		defer wg.Done()
		<-start
		for i := 0; i < 60; i++ {
			if err := a.SaveAtomic(); err != nil {
				t.Error(err)
				return
			}
			raw, err := os.ReadFile(a.FilePath)
			if err != nil {
				t.Error(err)
				return
			}
			value, err := Parse(raw)
			if err != nil {
				t.Error(err)
				return
			}
			check(value)
		}
	}()
	close(start)
	wg.Wait()
	if err := a.SaveAtomic(); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(a.FilePath)
	if err != nil {
		t.Fatal(err)
	}
	final, err := Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	if final.AccessToken != a.Snapshot().AccessToken {
		t.Fatal("final atomic save retained old credentials")
	}
	left, err := filepath.Glob(a.FilePath + ".tmp-*")
	if err != nil || len(left) != 0 {
		t.Fatalf("atomic save left temporary files: %v %v", left, err)
	}
}

func TestCredentialIntegritySaveFailurePreservesTargetAndCleansTemp(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "auth.json")
	original := []byte(`{"accessToken":"valid","uid":"u"}`)
	if err := os.WriteFile(path, original, 0600); err != nil {
		t.Fatal(err)
	}
	a := &Auth{UID: "u", FilePath: path}
	if err := a.SaveAtomic(); err == nil {
		t.Fatal("empty credentials were written")
	}
	if raw, err := os.ReadFile(path); err != nil || string(raw) != string(original) {
		t.Fatal("failed save overwrote existing credentials")
	}
	destination := filepath.Join(dir, "destination-directory")
	if err := os.Mkdir(destination, 0700); err != nil {
		t.Fatal(err)
	}
	a = &Auth{UID: "u", AccessToken: "valid", FilePath: destination}
	if err := a.SaveAtomic(); err == nil {
		t.Fatal("rename to directory unexpectedly succeeded")
	}
	if info, err := os.Stat(destination); err != nil || !info.IsDir() {
		t.Fatal("failed rename damaged target")
	}
	left, err := filepath.Glob(destination + ".tmp-*")
	if err != nil || len(left) != 0 {
		t.Fatalf("failed save leaked temp file: %v %v", left, err)
	}
}

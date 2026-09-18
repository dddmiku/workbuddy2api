// ═══ 更新日志 ═══
// 2026-09-18：复现热更新窗口的密钥跨实例丢写、撤销失效，以及返回切片对策略的意外修改。
package apikeys

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLiveStoresPreserveOtherInstanceChanges(t *testing.T) {
	path := filepath.Join(t.TempDir(), "keys.json")
	a, err := Open(path, "")
	if err != nil {
		t.Fatal(err)
	}
	b, err := Open(path, "")
	if err != nil {
		t.Fatal(err)
	}
	_, first, err := a.Create("first", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	_, second, err := b.Create("second", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	reloaded, err := Open(path, "")
	if err != nil {
		t.Fatal(err)
	}
	if !reloaded.Authenticate(first) || !reloaded.Authenticate(second) {
		t.Fatal("one live store overwrote the other's key")
	}
	if !a.Authenticate(second) {
		t.Fatal("live instance did not see newly committed key")
	}
}

func TestLiveStoresObserveRevocationImmediately(t *testing.T) {
	path := filepath.Join(t.TempDir(), "keys.json")
	a, err := Open(path, "")
	if err != nil {
		t.Fatal(err)
	}
	entry, key, err := a.Create("client", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	b, err := Open(path, "")
	if err != nil {
		t.Fatal(err)
	}
	disabled := false
	if _, err := b.Update(entry.ID, nil, nil, &disabled, nil); err != nil {
		t.Fatal(err)
	}
	if a.Authenticate(key) {
		t.Fatal("other live instance still accepts a disabled key")
	}
}

func TestLiveStoreFailsClosedOnCorruptedCredentialFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "keys.json")
	s, err := Open(path, "fixture-existing-key")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{corrupt"), 0600); err != nil {
		t.Fatal(err)
	}
	if s.Authenticate("fixture-existing-key") {
		t.Fatal("corrupt persistent key store silently fell back to stale authorization")
	}
}

func TestLiveStoreMetadataCannotMutateStoredModelPolicy(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "keys.json"), "")
	if err != nil {
		t.Fatal(err)
	}
	entry, key, err := s.Create("client", "", []string{"cn:allowed"})
	if err != nil {
		t.Fatal(err)
	}
	entry.Models[0] = "global:unexpected"
	s.List()[0].Models[0] = "another:unexpected"
	info, ok := s.Resolve(key)
	if !ok || len(info.Models) != 1 || info.Models[0] != "cn:allowed" {
		t.Fatalf("returned metadata changed stored policy: %+v", info.Models)
	}
}

func TestLiveStoreCanReopenEveryAllowedKeyWithModelBindings(t *testing.T) {
	path := filepath.Join(t.TempDir(), "keys.json")
	s, err := Open(path, "")
	if err != nil {
		t.Fatal(err)
	}
	models := make([]string, MaxBoundModels)
	for i := range models {
		models[i] = strings.Repeat("m", 62) + fmt.Sprintf("%02d", i)
	}
	keys := make([]record, MaxKeys)
	for i := range keys {
		keys[i] = record{Info: Info{ID: fmt.Sprintf("key_%024x", i), Name: "client", Models: models, Enabled: true}, Digest: digest(fmt.Sprintf("fixture-%d", i))}
	}
	if err := s.write(document{Version: 1, Keys: keys}); err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(path)
	if info.Size() <= 1<<20 {
		t.Fatal("fixture did not exceed the old limit")
	}
	if _, err := Open(path, ""); err != nil {
		t.Fatal(err)
	}
}

package apikeys

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// TestModelBindingValidationAndPersistence 覆盖模型白名单的校验、更新、清空与重载。
func TestModelBindingValidationAndPersistence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "api_keys.json")
	s, err := Open(path, "")
	if err != nil {
		t.Fatal(err)
	}
	info, key, err := s.Create("bound", "", []string{"cn:deepseek-v4.1-flash", "glm-5.2"})
	if err != nil {
		t.Fatal(err)
	}
	resolved, ok := s.Resolve(key)
	if !ok || len(resolved.Models) != 2 || resolved.Models[0] != "cn:deepseek-v4.1-flash" {
		t.Fatalf("models not stored: %+v", resolved.Models)
	}
	for _, bad := range [][]string{{""}, {"a b"}, {strings.Repeat("x", 65)}, {"dup", "dup"}, {"bad\nname"}} {
		if _, _, err := s.Create("x", "", bad); !errors.Is(err, ErrInvalidModels) {
			t.Errorf("invalid models accepted: %q (err=%v)", bad, err)
		}
	}
	too := make([]string, MaxBoundModels+1)
	for i := range too {
		too[i] = "m" + string(rune('a'+i%26)) + string(rune('a'+i/26))
	}
	if _, _, err := s.Create("x", "", too); !errors.Is(err, ErrInvalidModels) {
		t.Error("oversize model list accepted")
	}
	updated := []string{"glm-5.2"}
	if _, err := s.Update(info.ID, nil, nil, nil, &updated); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(path, "")
	if err != nil {
		t.Fatal(err)
	}
	entry, ok := reopened.Resolve(key)
	if !ok || len(entry.Models) != 1 || entry.Models[0] != "glm-5.2" {
		t.Fatalf("binding not persisted: %+v", entry.Models)
	}
	empty := []string{}
	if _, err := reopened.Update(info.ID, nil, nil, nil, &empty); err != nil {
		t.Fatal(err)
	}
	cleared, _ := reopened.Resolve(key)
	if len(cleared.Models) != 0 {
		t.Fatalf("binding not cleared: %+v", cleared.Models)
	}
}

func TestKeyLifecycleAndRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "api_keys.json")
	legacy := "old-existing-secret-keep-working"
	s, err := Open(path, legacy)
	if err != nil {
		t.Fatal(err)
	}
	if !s.Authenticate(legacy) || s.Authenticate("") {
		t.Fatal("legacy migration failed")
	}
	info, key, err := s.Create("测试客户端", "工作电脑", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(key, "wbk_") || len(key) != 47 || !s.Authenticate(key) {
		t.Fatal("new key not accepted")
	}
	raw, _ := os.ReadFile(path)
	if strings.Contains(string(raw), key) || strings.Contains(string(raw), legacy) {
		t.Fatal("plaintext key persisted")
	}
	stat, _ := os.Stat(path)
	if stat.Mode().Perm() != 0600 {
		t.Fatalf("permissions=%o", stat.Mode().Perm())
	}
	enabled := false
	if _, err = s.Update(info.ID, nil, nil, &enabled, nil); err != nil {
		t.Fatal(err)
	}
	if s.Authenticate(key) {
		t.Fatal("disabled key accepted")
	}
	s, err = Open(path, legacy)
	if err != nil {
		t.Fatal(err)
	}
	if s.Authenticate(key) || !s.Authenticate(legacy) {
		t.Fatal("restart lost enable state")
	}
	enabled = true
	name := "新的名称"
	if _, err = s.Update(info.ID, &name, nil, &enabled, nil); err != nil {
		t.Fatal(err)
	}
	if !s.Authenticate(key) {
		t.Fatal("reenabled key rejected")
	}
	if err = s.Delete(info.ID); err != nil {
		t.Fatal(err)
	}
	if err = s.Delete("legacy"); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path, legacy)
	if err != nil {
		t.Fatal(err)
	}
	if s.Authenticate(key) || s.Authenticate(legacy) || len(s.List()) != 0 {
		t.Fatal("deleted keys reactivated on restart")
	}
}

func TestPersistenceFailureDoesNotPublishMutation(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "keys.json"), "existing-secret-long-enough")
	if err != nil {
		t.Fatal(err)
	}
	s.persist = func(document) error { return errors.New("simulated disk failure") }
	if _, key, err := s.Create("new", "", nil); err == nil || key != "" {
		t.Fatal("failed write returned a usable key")
	}
	enabled := false
	if _, err = s.Update("legacy", nil, nil, &enabled, nil); err == nil {
		t.Fatal("write failure ignored")
	}
	if err = s.Delete("legacy"); err == nil {
		t.Fatal("delete failure ignored")
	}
	if len(s.List()) != 1 || !s.Authenticate("existing-secret-long-enough") {
		t.Fatal("failed write changed active state")
	}
}

func TestCorruptStoreFailsClosed(t *testing.T) {
	for _, data := range []string{`null`, `{}`, `{"version":2,"keys":[]}`, `{"version":1,"keys":[{"id":"legacy","name":"x","sha256":"bad"}]}`} {
		path := filepath.Join(t.TempDir(), "keys.json")
		if err := os.WriteFile(path, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := Open(path, "legacy-secret"); err == nil {
			t.Fatalf("corrupt store accepted: %s", data)
		}
	}
}

func TestKeyLabelValidation(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "keys.json"), "")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"", strings.Repeat("界", 65), "header\nvalue"} {
		if _, _, err := s.Create(name, "", nil); !errors.Is(err, ErrInvalid) {
			t.Fatalf("name accepted: %q", name)
		}
	}
	if _, _, err := s.Create("合法", strings.Repeat("x", 257), nil); !errors.Is(err, ErrInvalid) {
		t.Fatal("oversize note accepted")
	}
}

func TestConcurrentAuthenticationAndManagement(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "keys.json"), "")
	if err != nil {
		t.Fatal(err)
	}
	info, key, err := s.Create("concurrent", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				s.Authenticate(key)
				s.List()
			}
		}()
	}
	for i := 0; i < 12; i++ {
		enabled := i%2 == 0
		if _, err := s.Update(info.ID, nil, nil, &enabled, nil); err != nil {
			t.Fatal(err)
		}
	}
	wg.Wait()
}

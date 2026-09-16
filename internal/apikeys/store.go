// ═══ 更新日志 ═══
// 2026-09-16：增加持久化多密钥管理，保留原密钥并使启停、删除立即生效，只保存随机密钥的 SHA-256。
package apikeys

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"
)

const MaxKeys = 256

var (
	ErrNotFound = errors.New("密钥不存在或已删除")
	ErrInvalid  = errors.New("名称需为 1—64 字，备注不超过 256 字，且不能包含控制字符")
	ErrLimit    = errors.New("密钥数量已达上限，请先删除不再使用的密钥")
)

type Info struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Note      string    `json:"note"`
	MaskedKey string    `json:"masked_key"`
	Enabled   bool      `json:"enabled"`
	CreatedAt time.Time `json:"created_at"`
	Legacy    bool      `json:"legacy"`
}

type record struct {
	Info
	Digest string `json:"sha256"`
}

type document struct {
	Version int      `json:"version"`
	Keys    []record `json:"keys"`
}

type Store struct {
	mu      sync.RWMutex
	path    string
	keys    []record
	persist func(document) error
}

func Open(path, existingKey string) (*Store, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("API key file path is empty")
	}
	s := &Store{path: path, keys: []record{}}
	s.persist = s.write
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		if existingKey != "" {
			s.keys = append(s.keys, record{Info: Info{ID: "legacy", Name: "现有密钥", Note: "创建管理页前已在使用，原有客户端可继续使用", MaskedKey: mask(existingKey), Enabled: true, CreatedAt: time.Now().UTC(), Legacy: true}, Digest: digest(existingKey)})
		}
		if err := s.persist(document{Version: 1, Keys: s.keys}); err != nil {
			return nil, err
		}
		return s, nil
	}
	if err != nil {
		return nil, fmt.Errorf("open API keys: %w", err)
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, (1<<20)+1))
	if err != nil {
		return nil, err
	}
	if len(data) > 1<<20 {
		return nil, errors.New("API key file exceeds 1 MiB")
	}
	var doc document
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("invalid API key file: %w", err)
	}
	if doc.Version != 1 || len(doc.Keys) > MaxKeys {
		return nil, errors.New("unsupported API key file version or size")
	}
	seenIDs, seenDigests := map[string]bool{}, map[string]bool{}
	for _, key := range doc.Keys {
		decoded, e := hex.DecodeString(key.Digest)
		if e != nil || len(decoded) != sha256.Size || key.ID == "" || strings.ContainsAny(key.ID, "/\\") || seenIDs[key.ID] || seenDigests[key.Digest] || !validLabel(key.Name, key.Note) {
			return nil, errors.New("invalid or duplicate API key record")
		}
		seenIDs[key.ID], seenDigests[key.Digest] = true, true
	}
	if doc.Keys != nil {
		s.keys = doc.Keys
	}
	return s, nil
}

func digest(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}

func mask(key string) string {
	if len(key) < 16 {
		return "••••••••"
	}
	return key[:8] + "…" + key[len(key)-4:]
}

func validLabel(name, note string) bool {
	if name != strings.TrimSpace(name) || name == "" || utf8.RuneCountInString(name) > 64 || utf8.RuneCountInString(note) > 256 {
		return false
	}
	for _, r := range name + note {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

func (s *Store) Authenticate(key string) bool {
	if key == "" || len(key) > 512 {
		return false
	}
	want := digest(key)
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, entry := range s.keys {
		if entry.Enabled && subtle.ConstantTimeCompare([]byte(want), []byte(entry.Digest)) == 1 {
			return true
		}
	}
	return false
}

func (s *Store) List() []Info {
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := make([]Info, 0, len(s.keys))
	for _, entry := range s.keys {
		result = append(result, entry.Info)
	}
	return result
}

func (s *Store) Create(name, note string) (Info, string, error) {
	name, note = strings.TrimSpace(name), strings.TrimSpace(note)
	if !validLabel(name, note) {
		return Info{}, "", ErrInvalid
	}
	var raw [32]byte
	var id [12]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return Info{}, "", err
	}
	if _, err := rand.Read(id[:]); err != nil {
		return Info{}, "", err
	}
	key := "wbk_" + base64.RawURLEncoding.EncodeToString(raw[:])
	entry := record{Info: Info{ID: "key_" + hex.EncodeToString(id[:]), Name: name, Note: note, MaskedKey: mask(key), Enabled: true, CreatedAt: time.Now().UTC()}, Digest: digest(key)}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.keys) >= MaxKeys {
		return Info{}, "", ErrLimit
	}
	next := append(append([]record{}, s.keys...), entry)
	if err := s.commit(next); err != nil {
		return Info{}, "", err
	}
	return entry.Info, key, nil
}

func (s *Store) Update(id string, name, note *string, enabled *bool) (Info, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	next := append([]record{}, s.keys...)
	for i := range next {
		if next[i].ID != id {
			continue
		}
		if name != nil {
			next[i].Name = strings.TrimSpace(*name)
		}
		if note != nil {
			next[i].Note = strings.TrimSpace(*note)
		}
		if enabled != nil {
			next[i].Enabled = *enabled
		}
		if !validLabel(next[i].Name, next[i].Note) {
			return Info{}, ErrInvalid
		}
		if err := s.commit(next); err != nil {
			return Info{}, err
		}
		return next[i].Info, nil
	}
	return Info{}, ErrNotFound
}

func (s *Store) Delete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	next := make([]record, 0, len(s.keys))
	found := false
	for _, key := range s.keys {
		if key.ID == id {
			found = true
		} else {
			next = append(next, key)
		}
	}
	if !found {
		return ErrNotFound
	}
	return s.commit(next)
}

func (s *Store) commit(keys []record) error {
	if err := s.persist(document{Version: 1, Keys: keys}); err != nil {
		return err
	}
	s.keys = keys
	return nil
}

func (s *Store) write(doc document) error {
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".api-keys-*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if err = f.Chmod(0600); err == nil {
		err = json.NewEncoder(f).Encode(doc)
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if err = os.Rename(tmp, s.path); err != nil {
		return err
	}
	if d, e := os.Open(dir); e == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}

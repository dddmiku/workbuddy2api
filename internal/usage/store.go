// ═══ 更新日志 ═══
// 2026-09-17：新增按调用密钥累计的 token 用量账本：进程内累加、周期落盘，
//
//	供请求日志（key= 列）与管理台「用量统计」页读取。
package usage

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Version 账本文件格式版本。
const Version = 1

// defaultFlushInterval 落盘间隔：
// 账本是累计计数，进程崩溃最多丢一个窗口的数据，换来的是写盘次数与请求量解耦。
const defaultFlushInterval = 5 * time.Second

// Totals 一个维度的累计量。Credit 只在上游 usage 显式带 credit 时累加
// （缺失 ≠ 0，见 upstream 侧 Credit() 注释）。
type Totals struct {
	Requests         int64   `json:"requests"`
	PromptTokens     int64   `json:"prompt_tokens"`
	CompletionTokens int64   `json:"completion_tokens"`
	TotalTokens      int64   `json:"total_tokens"`
	Credit           float64 `json:"credit"`
}

// add 把一次请求计入累计量。
func (t *Totals) add(prompt, completion int, credit float64, hasCredit bool) {
	if prompt < 0 {
		prompt = 0
	}
	if completion < 0 {
		completion = 0
	}
	t.Requests++
	t.PromptTokens += int64(prompt)
	t.CompletionTokens += int64(completion)
	t.TotalTokens += int64(prompt + completion)
	if hasCredit {
		t.Credit += credit
	}
}

// ModelUsage 单模型维度。
type ModelUsage struct {
	Model  string `json:"model"`
	Totals Totals `json:"totals"`
}

// KeyUsage 单密钥维度（对外快照）。
type KeyUsage struct {
	KeyID       string       `json:"key_id"`
	Name        string       `json:"name"`
	MaskedKey   string       `json:"masked_key"`
	Totals      Totals       `json:"totals"`
	Models      []ModelUsage `json:"models"`
	FirstUsedAt time.Time    `json:"first_used_at"`
	LastUsedAt  time.Time    `json:"last_used_at"`
}

// Snapshot 一次读取结果：总量 + 按密钥明细（按总 token 降序）。
type Snapshot struct {
	Since     time.Time  `json:"since"`
	UpdatedAt time.Time  `json:"updated_at"`
	Totals    Totals     `json:"totals"`
	Keys      []KeyUsage `json:"keys"`
}

// keyRecord 落盘用的单密钥记录。
type keyRecord struct {
	Name        string             `json:"name"`
	MaskedKey   string             `json:"masked_key"`
	Totals      Totals             `json:"totals"`
	Models      map[string]*Totals `json:"models"`
	FirstUsedAt time.Time          `json:"first_used_at"`
	LastUsedAt  time.Time          `json:"last_used_at"`
}

// document 账本文件结构。
type document struct {
	Version   int                   `json:"version"`
	Since     time.Time             `json:"since"`
	UpdatedAt time.Time             `json:"updated_at"`
	Totals    Totals                `json:"totals"`
	Keys      map[string]*keyRecord `json:"keys"`
}

// Store 用量账本。零值不可用，必须经 Open 构造。
type Store struct {
	mu       sync.Mutex
	path     string
	interval time.Duration
	doc      document
	dirty    bool
	closed   bool
	stop     chan struct{}
	done     chan struct{}
	persist  func(document) error
}

// Open 打开（或新建）账本文件。path 为空返回错误——调用方据此跳过用量统计。
func Open(path string, interval time.Duration) (*Store, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("usage file path is empty")
	}
	if interval <= 0 {
		interval = defaultFlushInterval
	}
	s := &Store{
		path:     path,
		interval: interval,
		doc:      document{Version: Version, Since: time.Now().UTC(), Keys: map[string]*keyRecord{}},
		stop:     make(chan struct{}),
		done:     make(chan struct{}),
	}
	s.persist = s.write
	if err := s.load(); err != nil {
		return nil, err
	}
	go s.loop()
	return s, nil
}

// load 读取已有账本；文件不存在时保持空账本并落一次盘，保证目录里有可见文件。
func (s *Store) load() error {
	f, err := os.Open(s.path)
	if errors.Is(err, os.ErrNotExist) {
		s.dirty = true
		return s.Flush()
	}
	if err != nil {
		return err
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, (1<<20)+1))
	if err != nil {
		return err
	}
	if len(raw) > 1<<20 {
		return errors.New("usage file exceeds size limit")
	}
	if len(raw) == 0 {
		return nil
	}
	var doc document
	if err := json.Unmarshal(raw, &doc); err != nil {
		return err
	}
	if doc.Keys == nil {
		doc.Keys = map[string]*keyRecord{}
	}
	if doc.Since.IsZero() {
		doc.Since = time.Now().UTC()
	}
	doc.Version = Version
	s.doc = doc
	return nil
}

// Record 记一次成功请求的用量。keyID 为空时归到 "legacy"（单密钥模式/内置密钥）。
// model 为空时只计入密钥与总量维度。
func (s *Store) Record(keyID, name, maskedKey, model string, prompt, completion int, credit float64, hasCredit bool, at time.Time) {
	if s == nil {
		return
	}
	if strings.TrimSpace(keyID) == "" {
		keyID = "legacy"
	}
	if at.IsZero() {
		at = time.Now().UTC()
	} else {
		at = at.UTC()
	}
	if name == "" {
		name = "未命名密钥"
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	record := s.doc.Keys[keyID]
	if record == nil {
		record = &keyRecord{Models: map[string]*Totals{}, FirstUsedAt: at}
		s.doc.Keys[keyID] = record
	}
	// 名称/掩码以最近一次为准：管理台改名后页面要看到新名字。
	if name != "" {
		record.Name = name
	}
	if maskedKey != "" {
		record.MaskedKey = maskedKey
	}
	if record.FirstUsedAt.IsZero() {
		record.FirstUsedAt = at
	}
	record.LastUsedAt = at
	record.Totals.add(prompt, completion, credit, hasCredit)
	s.doc.Totals.add(prompt, completion, credit, hasCredit)
	s.doc.UpdatedAt = at
	if strings.TrimSpace(model) != "" {
		if record.Models == nil {
			record.Models = map[string]*Totals{}
		}
		counter := record.Models[model]
		if counter == nil {
			counter = &Totals{}
			record.Models[model] = counter
		}
		counter.add(prompt, completion, credit, hasCredit)
	}
	s.dirty = true
}

// Snapshot 返回当前累计值的深拷贝快照。
func (s *Store) Snapshot() Snapshot {
	if s == nil {
		return Snapshot{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := Snapshot{Since: s.doc.Since, UpdatedAt: s.doc.UpdatedAt, Totals: s.doc.Totals}
	out.Keys = make([]KeyUsage, 0, len(s.doc.Keys))
	for id, record := range s.doc.Keys {
		entry := KeyUsage{
			KeyID:       id,
			Name:        record.Name,
			MaskedKey:   record.MaskedKey,
			Totals:      record.Totals,
			FirstUsedAt: record.FirstUsedAt,
			LastUsedAt:  record.LastUsedAt,
		}
		models := make([]ModelUsage, 0, len(record.Models))
		for model, totals := range record.Models {
			models = append(models, ModelUsage{Model: model, Totals: *totals})
		}
		sort.Slice(models, func(i, j int) bool {
			if models[i].Totals.TotalTokens != models[j].Totals.TotalTokens {
				return models[i].Totals.TotalTokens > models[j].Totals.TotalTokens
			}
			return models[i].Model < models[j].Model
		})
		entry.Models = models
		out.Keys = append(out.Keys, entry)
	}
	sort.Slice(out.Keys, func(i, j int) bool {
		if out.Keys[i].Totals.TotalTokens != out.Keys[j].Totals.TotalTokens {
			return out.Keys[i].Totals.TotalTokens > out.Keys[j].Totals.TotalTokens
		}
		return out.Keys[i].KeyID < out.Keys[j].KeyID
	})
	return out
}

// Path 返回账本文件路径（页面展示用）。
func (s *Store) Path() string {
	if s == nil {
		return ""
	}
	return s.path
}

// Flush 把当前累计值原子落盘（tmp + rename）。
func (s *Store) Flush() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	if !s.dirty {
		s.mu.Unlock()
		return nil
	}
	snapshot := s.doc
	s.dirty = false
	s.mu.Unlock()

	snapshot.Version = Version
	if snapshot.UpdatedAt.IsZero() {
		snapshot.UpdatedAt = time.Now().UTC()
	}
	if err := s.persist(snapshot); err != nil {
		// 落盘失败：把脏标记放回去，下一轮重试（不覆盖已有文件）。
		s.mu.Lock()
		s.dirty = true
		s.mu.Unlock()
		return err
	}
	return nil
}

// write 原子写：先写同目录临时文件再 rename，避免半截 JSON 覆盖可用账本。
func (s *Store) write(doc document) error {
	if dir := filepath.Dir(s.path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
	}
	raw, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

// loop 周期落盘，直到 Close。
func (s *Store) loop() {
	defer close(s.done)
	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()
	for {
		select {
		case <-s.stop:
			return
		case <-ticker.C:
			_ = s.Flush()
		}
	}
}

// Close 停后台落盘并做最后一次落盘。
func (s *Store) Close() error {
	if s == nil || s.closed {
		return nil
	}
	s.closed = true
	close(s.stop)
	<-s.done
	return s.Flush()
}

// Reset 清空账本（管理台「清零」入口；保留文件与 Since 之外的结构）。
func (s *Store) Reset(at time.Time) error {
	if s == nil {
		return nil
	}
	if at.IsZero() {
		at = time.Now().UTC()
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.doc = document{Version: Version, Since: at.UTC(), Keys: map[string]*keyRecord{}}
	s.dirty = true
	s.mu.Unlock()
	return s.Flush()
}

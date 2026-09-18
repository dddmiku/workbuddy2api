// ═══ 更新日志 ═══
// 2026-09-17：新增按调用密钥累计的 token 用量账本：进程内累加、周期落盘，
//
//	供请求日志（key= 列）与管理台「用量统计」页读取。
//
// 2026-09-17：落盘前先与磁盘账本按字段取最大值合并。热更新时新旧进程会短暂同时
//
//	持有账本，合并保证两边记的请求都不丢，也不会把同一笔重复计两次。
//
// 2026-09-17：改为「基线 + 本方增量」并在文件锁内读改写。取最大值只在两侧看到同一批
//
//	记录时才对；两个进程各自服务不同请求时取大会丢掉一方记的请求，改增量后可累加。
//
// 2026-09-18：新增缓存命中输入维度（CachedTokens）。思考模式下每轮重发整段上下文，
// 输入里绝大部分是缓存命中；不单列出来，看总数会误以为「用了很多却只记了这么点」。
// 2026-09-18：文件锁覆盖整个读改写，隔离快照与在途增量，串行关闭/清零，拒绝覆盖损坏账本并支持合法大账本。
package usage

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Version 账本文件格式版本。
const Version = 1

const maxLedgerBytes = 64 << 20

// defaultFlushInterval 落盘间隔：
// 账本是累计计数，进程崩溃最多丢一个窗口的数据，换来的是写盘次数与请求量解耦。
const defaultFlushInterval = 5 * time.Second

// maxDayBuckets 账本保留的天桶数量（约 4 个月）。超过后裁掉最旧的天，账本不会无限增长。
const maxDayBuckets = 120

// dayKeyLayout 天桶键格式（服务端本地时区的日历日）。
const dayKeyLayout = "2006-01-02"

// dayKeyOf 把一次请求的时间折算成天桶键。
//
// 用本地时区而不是 UTC：面板按调用方所在时区显示日期，容器 TZ 与用户一致
// （compose 里 TZ=Asia/Shanghai）。按 UTC 分桶会把本地 00:00–08:00 的请求记到前一天。
func dayKeyOf(at time.Time) string {
	if at.IsZero() {
		at = time.Now()
	}
	return at.In(time.Local).Format(dayKeyLayout)
}

// Totals 一个维度的累计量。Credit 只在上游 usage 显式带 credit 时累加
// （缺失 ≠ 0，见 upstream 侧 Credit() 注释）。
type Totals struct {
	Requests         int64   `json:"requests"`
	PromptTokens     int64   `json:"prompt_tokens"`
	CachedTokens     int64   `json:"cached_tokens"`
	CompletionTokens int64   `json:"completion_tokens"`
	TotalTokens      int64   `json:"total_tokens"`
	Credit           float64 `json:"credit"`
}

// add 把一次请求计入累计量。prompt/completion/cached 为负表示上游没有该项观测（缺失≠0）。
func (t *Totals) add(prompt, completion, cached int, credit float64, hasCredit bool) {
	if prompt < 0 {
		prompt = 0
	}
	if completion < 0 {
		completion = 0
	}
	if cached < 0 {
		cached = 0
	}
	t.Requests++
	t.PromptTokens += int64(prompt)
	t.CachedTokens += int64(cached)
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
	KeyID     string       `json:"key_id"`
	Name      string       `json:"name"`
	MaskedKey string       `json:"masked_key"`
	Totals    Totals       `json:"totals"`
	Models    []ModelUsage `json:"models"`
	// Days 是该密钥的按天累计量（新到旧排序），面板按密钥筛日期时读它。
	Days        []DayUsage `json:"days"`
	FirstUsedAt time.Time  `json:"first_used_at"`
	LastUsedAt  time.Time  `json:"last_used_at"`
}

// Snapshot 一次读取结果：总量 + 按密钥明细（按总 token 降序）。
type Snapshot struct {
	Since     time.Time  `json:"since"`
	UpdatedAt time.Time  `json:"updated_at"`
	Totals    Totals     `json:"totals"`
	Keys      []KeyUsage `json:"keys"`
	// Days 是按天分桶的累计量（键为本地日历日 YYYY-MM-DD，新到旧排序），
	// 面板的日期筛选读它。
	Days []DayUsage `json:"days"`
}

// DayUsage 单日累计量。
type DayUsage struct {
	Day    string `json:"day"`
	Totals Totals `json:"totals"`
}

// keyRecord 落盘用的单密钥记录。
type keyRecord struct {
	Name        string             `json:"name"`
	MaskedKey   string             `json:"masked_key"`
	Totals      Totals             `json:"totals"`
	Models      map[string]*Totals `json:"models"`
	Days        map[string]*Totals `json:"days,omitempty"`
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
	Days      map[string]*Totals    `json:"days,omitempty"`
}

// Store 用量账本。零值不可用，必须经 Open 构造。
type Store struct {
	mu        sync.Mutex
	flushMu   sync.Mutex
	closeOnce sync.Once
	closeErr  error
	path      string
	interval  time.Duration
	doc       document
	// written 本进程上一次提交的累计值。落盘时用「当前 - written」算出本方新增量，
	// 热更新期间新旧进程各自只往盘上加自己那部分，既不覆盖对方也不重复计数。
	written document
	dirty   bool
	closed  bool
	stop    chan struct{}
	done    chan struct{}
	persist func(document) error
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
	doc, err := readLedger(s.path)
	if errors.Is(err, os.ErrNotExist) {
		s.dirty = true
		return s.Flush()
	}
	if err != nil {
		return err
	}
	if doc.Since.IsZero() {
		doc.Since = time.Now().UTC()
	}
	doc.Version = Version
	s.doc = doc
	s.written = cloneDocument(doc)
	return nil
}

// Record 记一次成功请求的用量。keyID 为空时归到 "legacy"（单密钥模式/内置密钥）。
// model 为空时只计入密钥与总量维度。
func (s *Store) Record(keyID, name, maskedKey, model string, prompt, completion, cached int, credit float64, hasCredit bool, at time.Time) {
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
	day := dayKeyOf(at)
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
	record.Totals.add(prompt, completion, cached, credit, hasCredit)
	s.doc.Totals.add(prompt, completion, cached, credit, hasCredit)
	s.doc.UpdatedAt = at
	// 天桶：总量与按密钥各记一份（面板按密钥筛选日期时要能对上）。
	if s.doc.Days == nil {
		s.doc.Days = map[string]*Totals{}
	}
	if dayTotals := s.doc.Days[day]; dayTotals != nil {
		dayTotals.add(prompt, completion, cached, credit, hasCredit)
	} else {
		created := &Totals{}
		created.add(prompt, completion, cached, credit, hasCredit)
		s.doc.Days[day] = created
	}
	if record.Days == nil {
		record.Days = map[string]*Totals{}
	}
	if keyDay := record.Days[day]; keyDay != nil {
		keyDay.add(prompt, completion, cached, credit, hasCredit)
	} else {
		created := &Totals{}
		created.add(prompt, completion, cached, credit, hasCredit)
		record.Days[day] = created
	}
	trimDayBuckets(s.doc.Days)
	trimDayBuckets(record.Days)
	if strings.TrimSpace(model) != "" {
		if record.Models == nil {
			record.Models = map[string]*Totals{}
		}
		counter := record.Models[model]
		if counter == nil {
			counter = &Totals{}
			record.Models[model] = counter
		}
		counter.add(prompt, completion, cached, credit, hasCredit)
	}
	s.dirty = true
}

// trimDayBuckets 只保留最近 maxDayBuckets 个天桶（键按字典序即时间序）。
func trimDayBuckets(buckets map[string]*Totals) {
	if len(buckets) <= maxDayBuckets {
		return
	}
	keys := make([]string, 0, len(buckets))
	for key := range buckets {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys[:len(keys)-maxDayBuckets] {
		delete(buckets, key)
	}
}

// Snapshot 返回当前累计值的深拷贝快照。
//
// 视图 = 盘上账本 + 本进程尚未落盘的新增量。为什么不能只看内存：热更新期间旧进程
// 收尾时会把在途请求记进同一个文件，新进程如果没有流量就不会再落盘，只看内存会让
// 面板一直少算那几条（实测 v1.3.4→v1.3.5 少算了 8256 token）。读盘失败时退回内存视图。
func (s *Store) Snapshot() Snapshot {
	if s == nil {
		return Snapshot{}
	}
	s.flushMu.Lock()
	defer s.flushMu.Unlock()
	s.mu.Lock()
	mine := cloneDocument(s.doc)
	written := cloneDocument(s.written)
	s.mu.Unlock()

	view := mine
	if disk, ok := s.readDisk(); ok {
		view = addDocument(disk, deltaDocument(mine, written))
	}

	out := Snapshot{Since: view.Since, UpdatedAt: view.UpdatedAt, Totals: view.Totals}
	out.Keys = make([]KeyUsage, 0, len(view.Keys))
	for id, record := range view.Keys {
		if record == nil {
			continue
		}
		entry := KeyUsage{
			KeyID:       id,
			Name:        record.Name,
			MaskedKey:   record.MaskedKey,
			Totals:      record.Totals,
			Days:        sortDays(record.Days),
			FirstUsedAt: record.FirstUsedAt,
			LastUsedAt:  record.LastUsedAt,
		}
		models := make([]ModelUsage, 0, len(record.Models))
		for model, totals := range record.Models {
			if totals == nil {
				continue
			}
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
	out.Days = sortDays(view.Days)
	return out
}

// sortDays 把天桶摊平成按日期倒序的切片（最新的一天在最前）。
func sortDays(buckets map[string]*Totals) []DayUsage {
	if len(buckets) == 0 {
		return []DayUsage{}
	}
	days := make([]DayUsage, 0, len(buckets))
	for day, totals := range buckets {
		if totals == nil {
			continue
		}
		days = append(days, DayUsage{Day: day, Totals: *totals})
	}
	sort.Slice(days, func(i, j int) bool { return days[i].Day > days[j].Day })
	return days
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
	s.flushMu.Lock()
	defer s.flushMu.Unlock()
	s.mu.Lock()
	if !s.dirty {
		s.mu.Unlock()
		return nil
	}
	snapshot := cloneDocument(s.doc)
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

// write 原子写：先在文件锁内把本进程的新增量并入盘上账本，再 tmp + rename。
func (s *Store) write(doc document) error {
	return s.persistDocument(doc, true)
}

// writeDirect 不做增量合并，直接覆盖（Reset 专用：清零必须真的清零）。
func (s *Store) writeDirect(doc document) error {
	return s.persistDocument(doc, false)
}

func (s *Store) persistDocument(doc document, merge bool) error {
	if dir := filepath.Dir(s.path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
	}
	unlock, err := lockLedger(s.path)
	if err != nil {
		return fmt.Errorf("lock usage ledger: %w", err)
	}
	defer unlock()
	snapshot := doc
	if merge {
		base, err := readLedger(s.path)
		if errors.Is(err, os.ErrNotExist) {
			base = s.writtenSnapshot()
		} else if err != nil {
			return fmt.Errorf("read usage ledger before merge: %w", err)
		}
		doc = addDocument(base, deltaDocument(snapshot, s.writtenSnapshot()))
	}
	raw, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	if len(raw) > maxLedgerBytes {
		return errors.New("usage file exceeds size limit")
	}
	file, err := os.CreateTemp(filepath.Dir(s.path), ".usage-*.tmp")
	if err != nil {
		return err
	}
	tmp := file.Name()
	defer os.Remove(tmp)
	if err = file.Chmod(0600); err == nil {
		_, err = file.Write(raw)
	}
	if err == nil {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if err := os.Rename(tmp, s.path); err != nil {
		return err
	}
	s.commit(doc, snapshot, merge)
	return nil
}

// mergeDelta 把「本进程自上次落盘以来的新增量」加到盘上的账本里。
//
// 为什么不是简单覆盖：热更新期间新旧两个进程会同时存活（新实例已接管监听，旧实例
// 还在把在途请求跑完），谁后落盘谁就会用自己内存里的快照覆盖对方刚写的记录。
//
// 为什么也不是"逐字段取最大值"：取大只对"两侧看到的是同一批记录"成立。两个进程
// 各自服务不同请求时，取大会**丢掉**只被一方记到的那部分（旧进程 100 笔 + 新进程
// 5 笔 → 仍是 100 笔）。改成"基线 + 本方增量"后，两边记录的都会累加，且由于增量
// 是本方累计值减去本方上次已提交的累计值，重复落盘也不会重复计数。
//
// persistDocument 在持有文件锁时完成全部读改写，读取失败保留原文件与待写增量。
func readLedger(path string) (document, error) {
	empty := document{Version: Version, Keys: map[string]*keyRecord{}}
	file, err := os.Open(path)
	if err != nil {
		return empty, err
	}
	defer file.Close()
	raw, err := io.ReadAll(io.LimitReader(file, maxLedgerBytes+1))
	if err != nil {
		return empty, err
	}
	if len(raw) > maxLedgerBytes {
		return empty, errors.New("usage file exceeds size limit")
	}
	var disk document
	if err := json.Unmarshal(raw, &disk); err != nil {
		return empty, err
	}
	if disk.Version != Version {
		return empty, errors.New("unsupported usage file version")
	}
	if disk.Keys == nil {
		disk.Keys = map[string]*keyRecord{}
	}
	return disk, nil
}

// readDisk 用于只读视图；读盘失败时 Snapshot 保留内存中的已知记录。
func (s *Store) readDisk() (document, bool) {
	doc, err := readLedger(s.path)
	return doc, err == nil
}

// writtenSnapshot 本进程上次提交的累计值（深拷贝，防止后续写入改到它）。
func (s *Store) writtenSnapshot() document {
	s.mu.Lock()
	defer s.mu.Unlock()
	return cloneDocument(s.written)
}

// commit 记录本次落盘后的账本基线。
//
// adoptDisk 为真（增量合并路径）说明盘上可能还有别的进程的贡献：把这些贡献加进内存视图，
// 面板才能立刻看到合并后的真实数字；同时把基线提到合并结果，下一次的新增量只算本方
// 新记录，不会重复计入别人的部分。
func (s *Store) commit(mine, snapshot document, adoptDisk bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !adoptDisk {
		s.written = cloneDocument(mine)
		return
	}
	// Records accepted after the flush snapshot were not persisted by this
	// write. Carry them forward separately from other processes' contributions.
	pending := deltaDocument(s.doc, snapshot)
	s.doc = addDocument(cloneDocument(mine), pending)
	s.written = cloneDocument(mine)
}

// deltaDocument 本进程自上次提交以来的新增量（累计量单调不减，负数按 0 处理）。
func deltaDocument(mine, prev document) document {
	out := document{
		Version:   Version,
		Since:     mine.Since,
		UpdatedAt: mine.UpdatedAt,
		Totals:    deltaTotals(mine.Totals, prev.Totals),
		Keys:      make(map[string]*keyRecord, len(mine.Keys)),
		Days:      deltaDays(mine.Days, prev.Days),
	}
	for id, current := range mine.Keys {
		if current == nil {
			continue
		}
		var before keyRecord
		if old := prev.Keys[id]; old != nil {
			before = *old
		}
		item := &keyRecord{
			Name:        current.Name,
			MaskedKey:   current.MaskedKey,
			Totals:      deltaTotals(current.Totals, before.Totals),
			Models:      make(map[string]*Totals, len(current.Models)),
			Days:        deltaDays(current.Days, before.Days),
			FirstUsedAt: current.FirstUsedAt,
			LastUsedAt:  current.LastUsedAt,
		}
		for model, totals := range current.Models {
			if totals == nil {
				continue
			}
			var beforeModel Totals
			if before.Models != nil {
				if old := before.Models[model]; old != nil {
					beforeModel = *old
				}
			}
			delta := deltaTotals(*totals, beforeModel)
			item.Models[model] = &delta
		}
		out.Keys[id] = item
	}
	return out
}

// deltaDays 逐天算增量；对方没有的天（本进程新记录）整体作为增量保留。
func deltaDays(mine, prev map[string]*Totals) map[string]*Totals {
	out := make(map[string]*Totals, len(mine))
	for day, totals := range mine {
		if totals == nil {
			continue
		}
		var before Totals
		if old := prev[day]; old != nil {
			before = *old
		}
		delta := deltaTotals(*totals, before)
		out[day] = &delta
	}
	return out
}

// addDocument 把增量并进基线账本（总量、按密钥、按模型逐层累加）。
func addDocument(base, delta document) document {
	base.Version = Version
	base.Totals = addTotals(base.Totals, delta.Totals)
	if !delta.Since.IsZero() && (base.Since.IsZero() || delta.Since.Before(base.Since)) {
		base.Since = delta.Since
	}
	if delta.UpdatedAt.After(base.UpdatedAt) {
		base.UpdatedAt = delta.UpdatedAt
	}
	if base.Keys == nil {
		base.Keys = map[string]*keyRecord{}
	}
	base.Days = addDays(base.Days, delta.Days)
	for id, add := range delta.Keys {
		if add == nil {
			continue
		}
		current := base.Keys[id]
		if current == nil {
			copied := *add
			copied.Models = cloneModels(add.Models)
			copied.Days = cloneModels(add.Days)
			base.Keys[id] = &copied
			continue
		}
		current.Totals = addTotals(current.Totals, add.Totals)
		if add.Name != "" && (current.Name == "" || !add.LastUsedAt.Before(current.LastUsedAt)) {
			current.Name = add.Name
		}
		if add.MaskedKey != "" && (current.MaskedKey == "" || !add.LastUsedAt.Before(current.LastUsedAt)) {
			current.MaskedKey = add.MaskedKey
		}
		if !add.FirstUsedAt.IsZero() && (current.FirstUsedAt.IsZero() || add.FirstUsedAt.Before(current.FirstUsedAt)) {
			current.FirstUsedAt = add.FirstUsedAt
		}
		if add.LastUsedAt.After(current.LastUsedAt) {
			current.LastUsedAt = add.LastUsedAt
		}
		if current.Models == nil {
			current.Models = map[string]*Totals{}
		}
		for model, totals := range add.Models {
			if totals == nil {
				continue
			}
			if existing := current.Models[model]; existing != nil {
				merged := addTotals(*existing, *totals)
				current.Models[model] = &merged
			} else {
				copied := *totals
				current.Models[model] = &copied
			}
		}
		if current.Days == nil {
			current.Days = map[string]*Totals{}
		}
		for day, totals := range add.Days {
			if totals == nil {
				continue
			}
			if existing := current.Days[day]; existing != nil {
				merged := addTotals(*existing, *totals)
				current.Days[day] = &merged
			} else {
				copied := *totals
				current.Days[day] = &copied
			}
		}
		trimDayBuckets(current.Days)
	}
	trimDayBuckets(base.Days)
	return base
}

// addDays 逐天累加两个天桶表。
func addDays(base, delta map[string]*Totals) map[string]*Totals {
	if len(base) == 0 && len(delta) == 0 {
		return base
	}
	if base == nil {
		base = map[string]*Totals{}
	}
	for day, totals := range delta {
		if totals == nil {
			continue
		}
		if existing := base[day]; existing != nil {
			merged := addTotals(*existing, *totals)
			base[day] = &merged
		} else {
			copied := *totals
			base[day] = &copied
		}
	}
	return base
}

// cloneDocument 深拷贝账本，避免共享 map / 指针。
func cloneDocument(doc document) document {
	out := doc
	out.Keys = make(map[string]*keyRecord, len(doc.Keys))
	out.Days = cloneModels(doc.Days)
	for id, record := range doc.Keys {
		if record == nil {
			continue
		}
		copied := *record
		copied.Models = cloneModels(record.Models)
		copied.Days = cloneModels(record.Days)
		out.Keys[id] = &copied
	}
	return out
}

// deltaTotals 逐字段算增量，负数（清零或跨进程读到的更大值）按 0 处理。
func deltaTotals(now, prev Totals) Totals {
	return Totals{
		Requests:         positive(now.Requests - prev.Requests),
		PromptTokens:     positive(now.PromptTokens - prev.PromptTokens),
		CachedTokens:     positive(now.CachedTokens - prev.CachedTokens),
		CompletionTokens: positive(now.CompletionTokens - prev.CompletionTokens),
		TotalTokens:      positive(now.TotalTokens - prev.TotalTokens),
		Credit:           positiveFloat(now.Credit - prev.Credit),
	}
}

// addTotals 逐字段相加。
func addTotals(base, delta Totals) Totals {
	return Totals{
		Requests:         base.Requests + delta.Requests,
		PromptTokens:     base.PromptTokens + delta.PromptTokens,
		CachedTokens:     base.CachedTokens + delta.CachedTokens,
		CompletionTokens: base.CompletionTokens + delta.CompletionTokens,
		TotalTokens:      base.TotalTokens + delta.TotalTokens,
		Credit:           base.Credit + delta.Credit,
	}
}

func positive(value int64) int64 {
	if value < 0 {
		return 0
	}
	return value
}

func positiveFloat(value float64) float64 {
	if value < 0 {
		return 0
	}
	return value
}

// cloneModels 深拷贝按模型维度的累计量。
func cloneModels(source map[string]*Totals) map[string]*Totals {
	if source == nil {
		return map[string]*Totals{}
	}
	out := make(map[string]*Totals, len(source))
	for model, totals := range source {
		if totals == nil {
			continue
		}
		copied := *totals
		out[model] = &copied
	}
	return out
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
			if err := s.Flush(); err != nil {
				log.Printf("WARN: [usage] ledger flush failed: %v", err)
			}
		}
	}
}

// Close 停后台落盘并做最后一次落盘。
func (s *Store) Close() error {
	if s == nil {
		return nil
	}
	s.closeOnce.Do(func() {
		s.mu.Lock()
		s.closed = true
		close(s.stop)
		s.mu.Unlock()
		<-s.done
		s.closeErr = s.Flush()
	})
	return s.closeErr
}

// Reset 清空账本（管理台「清零」入口；保留文件与 Since 之外的结构）。
func (s *Store) Reset(at time.Time) error {
	if s == nil {
		return nil
	}
	if at.IsZero() {
		at = time.Now().UTC()
	}
	s.flushMu.Lock()
	defer s.flushMu.Unlock()
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	previous, written, dirty := s.doc, s.written, s.dirty
	s.doc = document{Version: Version, Since: at.UTC(), Keys: map[string]*keyRecord{}}
	s.written = cloneDocument(s.doc)
	s.dirty = false
	snapshot := cloneDocument(s.doc)
	s.mu.Unlock()
	// 清零必须直接覆盖：走增量合并的话，盘上旧数据会被当成"别人的贡献"保留下来。
	// 清零之后各进程的新增量照旧累加（它们只减自己上次提交的基线）。
	if err := s.writeDirect(snapshot); err != nil {
		s.mu.Lock()
		s.doc = addDocument(previous, s.doc)
		s.written = written
		s.dirty = dirty || s.dirty
		s.mu.Unlock()
		return err
	}
	return nil
}

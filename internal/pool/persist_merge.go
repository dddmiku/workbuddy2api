// ═══ 更新日志 ═══
// 2026-09-18：按本实例基线合并持久化字段增量，记录显式赋值意图，防止旧进程最后落盘覆盖新状态。
// 2026-09-18：导入快照计数单独作为下限，创建意图带删除代次，避免重复计数与删除后复活。
package pool

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"os"
)

type stateIntent struct {
	fields           map[string]bool
	models           map[string]bool
	deltas           map[string]int64
	created          bool
	deleted          bool
	createdEpoch     uint64
	deletedEpoch     uint64
	importedCounters bool
	importedSuccess  int64
	importedErrors   int64
}

func cloneAccountEpochs(source map[string]uint64) map[string]uint64 {
	if len(source) == 0 {
		return nil
	}
	copy := make(map[string]uint64, len(source))
	for uid, epoch := range source {
		copy[uid] = epoch
	}
	return copy
}

func (p *Pool) observeAccountEpochLocked(uid string) (uint64, bool, bool) {
	epoch := p.persistBase.AccountEpochs[uid]
	if p.stateFp == "" {
		return epoch, false, false
	}
	raw, err := os.ReadFile(p.stateFp)
	if os.IsNotExist(err) {
		return epoch, false, true
	}
	if err != nil {
		return epoch, false, false
	}
	var state struct {
		Accounts      map[string]json.RawMessage `json:"accounts"`
		AccountEpochs map[string]uint64          `json:"account_epochs"`
	}
	if json.Unmarshal(raw, &state) != nil {
		return epoch, false, false
	}
	_, exists := state.Accounts[uid]
	return max(epoch, state.AccountEpochs[uid]), exists, true
}

func (p *Pool) stateIntentLocked(uid string) *stateIntent {
	if p.stateIntents == nil {
		p.stateIntents = make(map[string]*stateIntent)
	}
	change := p.stateIntents[uid]
	if change == nil {
		change = &stateIntent{fields: map[string]bool{}, models: map[string]bool{}, deltas: map[string]int64{}}
		p.stateIntents[uid] = change
	}
	return change
}

func (p *Pool) markStateFieldsLocked(uid string, fields ...string) {
	if p.stateFp != "" {
		change := p.stateIntentLocked(uid)
		for _, field := range fields {
			change.fields[field] = true
		}
	}
	p.dirty.Store(true)
}

func (p *Pool) markStateModelLocked(uid, model string) {
	if p.stateFp != "" {
		p.stateIntentLocked(uid).models[model] = true
	}
	p.dirty.Store(true)
}

func (p *Pool) addStateDeltaLocked(uid, field string, delta int64) {
	if delta == 0 {
		return
	}
	if p.stateFp != "" {
		p.stateIntentLocked(uid).deltas[field] += delta
	}
	p.dirty.Store(true)
}

func stateFields(value any) (map[string]json.RawMessage, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	var fields map[string]json.RawMessage
	err = json.Unmarshal(raw, &fields)
	return fields, err
}

func rawFields(raw json.RawMessage) map[string]json.RawMessage {
	var fields map[string]json.RawMessage
	_ = json.Unmarshal(raw, &fields)
	if fields == nil {
		fields = make(map[string]json.RawMessage)
	}
	return fields
}

func mergeStateFields(base, current, latest map[string]json.RawMessage, forced map[string]bool) {
	for key, previous := range base {
		if _, exists := current[key]; !exists && bytes.Equal(latest[key], previous) {
			delete(latest, key)
		}
	}
	for key, value := range current {
		if !bytes.Equal(value, base[key]) {
			latest[key] = value
		}
	}
	for key := range forced {
		if value, exists := current[key]; exists {
			latest[key] = value
		} else {
			delete(latest, key)
		}
	}
}

func mergedCounter(latest, current, base int64) (int64, error) {
	if current < base {
		return current, nil
	}
	delta := current - base
	if delta > 0 && latest > math.MaxInt64-delta {
		return 0, fmt.Errorf("persisted account counter overflow")
	}
	return latest + delta, nil
}

func mergeStateAccount(base, current, latest stateAccount, intent *stateIntent) (stateAccount, error) {
	b, err := stateFields(base)
	if err != nil {
		return stateAccount{}, err
	}
	c, err := stateFields(current)
	if err != nil {
		return stateAccount{}, err
	}
	d, err := stateFields(latest)
	if err != nil {
		return stateAccount{}, err
	}
	if intent == nil {
		intent = &stateIntent{}
	}
	models := rawFields(d["model_cooldowns"])
	mergeStateFields(rawFields(b["model_cooldowns"]), rawFields(c["model_cooldowns"]), models, intent.models)
	mergeStateFields(b, c, d, intent.fields)
	if !intent.fields["model_cooldowns"] {
		if len(models) == 0 {
			delete(d, "model_cooldowns")
		} else {
			d["model_cooldowns"], err = json.Marshal(models)
			if err != nil {
				return stateAccount{}, err
			}
		}
	}
	raw, err := json.Marshal(d)
	if err != nil {
		return stateAccount{}, err
	}
	var result stateAccount
	if err := json.Unmarshal(raw, &result); err != nil {
		return stateAccount{}, err
	}
	latestSuccess, latestErrors := latest.SuccessCount, max(latest.ErrTotal, int64(latest.ErrCount))
	if intent.importedCounters {
		latestSuccess = max(latestSuccess, intent.importedSuccess)
		latestErrors = max(latestErrors, intent.importedErrors)
	}
	result.SuccessCount, err = mergedCounter(latestSuccess, current.SuccessCount, base.SuccessCount)
	if err != nil {
		return stateAccount{}, err
	}
	result.ErrTotal, err = mergedCounter(latestErrors, current.ErrTotal, max(base.ErrTotal, int64(base.ErrCount)))
	if err != nil {
		return stateAccount{}, err
	}
	if latest.LastSuccess.After(result.LastSuccess) {
		result.LastSuccess = latest.LastSuccess
	}
	if latest.LastErr.After(result.LastErr) {
		result.LastErr = latest.LastErr
	}
	if !intent.fields["credits"] && intent.deltas["credits"] != 0 {
		result.Credits = max(0, latest.Credits+intent.deltas["credits"])
	}
	if !intent.fields["credits_expiring"] && intent.deltas["credits_expiring"] != 0 {
		result.CreditsExpiring = max(0, latest.CreditsExpiring+intent.deltas["credits_expiring"])
	}
	return result, nil
}

// 调用方已持有进程内状态锁和同文件跨进程锁；仅读取磁盘，不把别的实例运行态写回当前租约对象。
func (p *Pool) mergedStateLocked(current stateFile) (stateFile, error) {
	raw, err := os.ReadFile(p.stateFp)
	missing := os.IsNotExist(err)
	if err != nil && !missing {
		return stateFile{}, err
	}
	var latest stateFile
	if missing {
		latest = current
		latest.AccountEpochs = cloneAccountEpochs(current.AccountEpochs)
	} else if err := json.Unmarshal(raw, &latest); err != nil {
		return stateFile{}, fmt.Errorf("read existing pool state: %w", err)
	}
	if latest.Accounts == nil {
		latest.Accounts = make(map[string]stateAccount)
	}
	if latest.AccountEpochs == nil {
		latest.AccountEpochs = make(map[string]uint64)
	}
	for uid, epoch := range current.AccountEpochs {
		latest.AccountEpochs[uid] = max(latest.AccountEpochs[uid], epoch)
	}
	for uid, intent := range p.stateIntents {
		if intent.deleted {
			if intent.deletedEpoch != 0 {
				if latest.AccountEpochs[uid] > intent.deletedEpoch {
					continue
				}
				latest.AccountEpochs[uid] = intent.deletedEpoch
			} else {
				if latest.AccountEpochs[uid] == ^uint64(0) {
					return stateFile{}, fmt.Errorf("account deletion epoch overflow")
				}
				latest.AccountEpochs[uid]++
			}
			delete(latest.Accounts, uid)
		}
	}
	if missing {
		return latest, nil
	}
	for uid, account := range current.Accounts {
		base, known := p.persistBase.Accounts[uid]
		fresh, exists := latest.Accounts[uid]
		intent := p.stateIntents[uid]
		if intent != nil && intent.created && intent.createdEpoch != latest.AccountEpochs[uid] {
			continue
		}
		if !exists {
			if !known || (intent != nil && intent.created) {
				latest.Accounts[uid] = account
			}
			continue
		}
		merged, err := mergeStateAccount(base, account, fresh, intent)
		if err != nil {
			return stateFile{}, err
		}
		latest.Accounts[uid] = merged
	}
	return latest, nil
}

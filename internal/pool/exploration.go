// ═══ 更新日志 ═══
// 2026-09-19：给未知可用账号有界轮询机会，同时保留免费优先和会话亲和。
package pool

import "crypto/sha256"

const maxExplorationScopes = 256

type explorationState struct {
	preferred uint8
	cursor    string
	touched   uint64
}

// exploreUnknownLocked is called only after realm, health, model cooldown,
// in-flight and retry exclusions. It never revives or probes an ineligible account.
// Keep only a cursor per model/realm (not per request or caller), with an LRU cap.
func (p *Pool) exploreUnknownLocked(unknown []*entry, model, realm string) *entry {
	key := sha256.Sum256([]byte(realm + "\x00" + model))
	if p.costExploration == nil {
		p.costExploration = make(map[[32]byte]*explorationState)
	}
	state := p.costExploration[key]
	if state == nil {
		if len(p.costExploration) >= maxExplorationScopes {
			var oldest [32]byte
			var seq uint64 = ^uint64(0)
			for k, s := range p.costExploration {
				if s.touched < seq {
					oldest, seq = k, s.touched
				}
			}
			delete(p.costExploration, oldest)
		}
		state = &explorationState{}
		p.costExploration[key] = state
	}
	state.touched = p.pickSeq + 1
	state.preferred++
	if state.preferred < 4 {
		return nil
	}
	state.preferred = 0
	// UID order gives a stable round-robin independent of global lastUsed:
	// heavy sticky traffic must not keep an unknown account out of exploration.
	var next, first *entry
	for _, e := range unknown {
		uid := e.a.UID
		if first == nil || uid < first.a.UID {
			first = e
		}
		if uid > state.cursor && (next == nil || uid < next.a.UID) {
			next = e
		}
	}
	if next == nil {
		next = first
	}
	state.cursor = next.a.UID
	return next
}

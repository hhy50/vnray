package vmess

// 请求指纹判重：以 user+bodyKey+bodyIV 识别重放与客户端侧密钥复用，补上
// AuthID 判重覆盖不到的场景（同指纹换 AuthID、客户端生成器缺陷导致的
// IV/Key 复用）；键布局与过期语义对照 v2fly SessionHistory。

import (
	"sync"
	"sync/atomic"
	"time"
)

// sessionHistoryTTL 是同一指纹被记为重放的窗口，v2fly 同值 3 分钟。
const sessionHistoryTTL = 3 * time.Minute

// sessionKey 是一条已受理请求的指纹：UUID 16B + 请求体密钥 16B + 请求体 IV 16B。
type sessionKey [48]byte

// sessionHistory 记录近期受理过的请求指纹，TTL 内重复出现即判重放。
type sessionHistory struct {
	seen   sync.Map
	lastGC atomic.Int64
}

// newSessionHistory 创建空的会话判重表。
func newSessionHistory() *sessionHistory {
	f := &sessionHistory{}
	f.lastGC.Store(time.Now().UnixNano())
	return f
}

// Check 登记并校验一条请求指纹：TTL 内重复出现返回 false；已过期条目视为
// 新请求，覆盖时间戳并放行。GC 惰性触发，与 replayFilter 同策略。
func (h *sessionHistory) Check(user [16]byte, bodyKey, bodyIV [16]byte) bool {
	var key sessionKey
	copy(key[0:16], user[:])
	copy(key[16:32], bodyKey[:])
	copy(key[32:48], bodyIV[:])
	now := time.Now()
	for {
		previous, loaded := h.seen.LoadOrStore(key, now)
		if !loaded {
			break
		}
		if now.Sub(previous.(time.Time)) < sessionHistoryTTL {
			return false
		}
		if h.seen.CompareAndSwap(key, previous, now) {
			break
		}
	}
	last := h.lastGC.Load()
	if now.UnixNano()-last > int64(time.Minute) && h.lastGC.CompareAndSwap(last, now.UnixNano()) {
		h.seen.Range(func(key, value any) bool {
			if now.Sub(value.(time.Time)) >= sessionHistoryTTL {
				h.seen.CompareAndDelete(key, value)
			}
			return true
		})
	}
	return true
}

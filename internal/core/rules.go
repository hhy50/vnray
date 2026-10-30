package core

// rules.go — 用户出站规则的服务端实现：规则定义存在服务端配置表（键 rule.<name>），
// 目标判定也只在服务端做。写路径是"先校验 → 落库 → 换内存快照"，读路径只读快照，
// 因此改规则对已建立的会话同样即时生效（判定发生在每条 Forward 上）。

import (
	"context"
	"strings"
	"sync"

	"github.com/hhy50/vnray/internal/logging"
	"github.com/hhy50/vnray/internal/rule"
	"github.com/hhy50/vnray/internal/store"
)

// ruleSet 是用户出站规则的运行时持有者，实现 admin.RuleSettings。
type ruleSet struct {
	mu       sync.RWMutex
	compiled map[string]*rule.Compiled
	settings store.Store
}

// ruleSeededKey 标记预置规则已写过一次。它不匹配 rule. 前缀，因此不会被当成规则解析；
// 有了这个标记，把规则删光之后重启也不会再冒出一条 default。
const ruleSeededKey = "rules.seeded"

// legacyKeyPrefix 是旧版规则键前缀（user.rule.<name>）：启动时迁到 rule.<name> 并删掉旧行。
const legacyKeyPrefix = "user.rule."

// loadRuleSet 读取配置表里的规则，并在首次启动时写入预置规则。此后以库为准：删掉的
// 规则不会在重启后复活。单条规则解析失败只告警并跳过，不拒绝启动。
func loadRuleSet(ctx context.Context, st store.Store) *ruleSet {
	rs := &ruleSet{compiled: map[string]*rule.Compiled{}, settings: st}
	kv, err := st.GetServiceSettings(ctx)
	if err != nil {
		logging.Warnf("user rules: load failed, using built-in defaults: %v", err)
		for name, r := range rule.Defaults() {
			if c, cerr := rule.Compile(name, r); cerr == nil {
				rs.compiled[name] = c
			}
		}
		return rs
	}
	migrateLegacyKeys(ctx, st, kv)
	if _, seeded := kv[ruleSeededKey]; !seeded {
		seed := map[string]string{ruleSeededKey: "true"}
		for name, r := range rule.Defaults() {
			key := rule.Key(name)
			// 老库已有这条规则（可能改过名单）：补标记，不覆盖用户数据。
			if _, exists := kv[key]; exists {
				continue
			}
			raw, err := rule.Encode(r)
			if err != nil {
				continue
			}
			seed[key] = raw
			kv[key] = raw
		}
		if err := st.SetServiceSettings(ctx, seed); err != nil {
			logging.Warnf("user rules: seed default rule failed: %v", err)
		} else {
			logging.Info("user rules: default rule seeded", "count", len(seed)-1)
		}
	}
	for key, raw := range kv {
		name, ok := rule.NameFromKey(key)
		if !ok {
			continue
		}
		r, err := rule.Parse(raw)
		if err != nil {
			logging.Warnf("user rules: skip %s: %v", name, err)
			continue
		}
		c, err := rule.Compile(name, r)
		if err != nil {
			logging.Warnf("user rules: skip %s: %v", name, err)
			continue
		}
		rs.compiled[name] = c
	}
	return rs
}

// Verdict 返回目标 host 在指定规则下的判定。规则名为空按内置 default 处理；
// 规则不存在（被删或从未定义）时放行——宁可退回默认转发，也不要让用户的流量全断。
func (s *ruleSet) Verdict(name, host string) (rule.Verdict, string) {
	if s == nil {
		return rule.Pass, ""
	}
	if name == "" {
		name = rule.NameDefault
	}
	s.mu.RLock()
	c := s.compiled[name]
	s.mu.RUnlock()
	if c == nil {
		return rule.Pass, name
	}
	return c.Verdict(host), name
}

// List 返回全部规则（按名字升序）。
func (s *ruleSet) List() []rule.Named {
	if s == nil {
		return nil
	}
	s.mu.RLock()
	out := make([]rule.Named, 0, len(s.compiled))
	for name, c := range s.compiled {
		out = append(out, rule.Named{Name: name, Rule: c.Rule()})
	}
	s.mu.RUnlock()
	rule.SortNamed(out)
	return out
}

// Exists 报告规则名是否已定义。
func (s *ruleSet) Exists(name string) bool {
	if s == nil {
		return false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	_, ok := s.compiled[name]
	return ok
}

// SaveRule 校验、落库并换内存：校验失败不落库，落库失败不换内存。
func (s *ruleSet) SaveRule(name string, r rule.Rule) error {
	c, err := rule.Compile(name, r)
	if err != nil {
		return err
	}
	raw, err := rule.Encode(r)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), applyTimeout)
	defer cancel()
	if err := s.settings.SetServiceSettings(ctx, map[string]string{rule.Key(name): raw}); err != nil {
		return err
	}
	s.mu.Lock()
	s.compiled[name] = c
	s.mu.Unlock()
	return nil
}

// DeleteRule 删除一条规则（含 default）：引用它的用户会退回"不干预"，重启不会补回来。
func (s *ruleSet) DeleteRule(name string) error {
	if err := rule.ValidName(name); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), applyTimeout)
	defer cancel()
	if err := s.settings.SetServiceSettings(ctx, map[string]string{rule.Key(name): ""}); err != nil {
		return err
	}
	s.mu.Lock()
	delete(s.compiled, name)
	s.mu.Unlock()
	return nil
}

// migrateLegacyKeys 把旧前缀的规则行搬到 rule.<name>：新键已存在时以新键为准，只删旧行。
// 迁移与删除在同一批写入里，避免半套键值被读到。
func migrateLegacyKeys(ctx context.Context, st store.Store, kv map[string]string) {
	updates := map[string]string{}
	migrated := 0
	for key, raw := range kv {
		if !strings.HasPrefix(key, legacyKeyPrefix) {
			continue
		}
		name := strings.TrimPrefix(key, legacyKeyPrefix)
		if name == "" {
			continue
		}
		// 空值行本来就是"已删除"，只清掉旧键。
		if raw != "" {
			target := rule.Key(name)
			if _, exists := kv[target]; !exists {
				updates[target] = raw
				kv[target] = raw
				migrated++
			}
		}
		updates[key] = ""
		delete(kv, key)
	}
	if len(updates) == 0 {
		return
	}
	if err := st.SetServiceSettings(ctx, updates); err != nil {
		logging.Warnf("user rules: migrate legacy keys failed: %v", err)
		return
	}
	logging.Info("user rules: legacy keys migrated", "migrated", migrated)
}

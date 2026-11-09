package core

// rules.go — 用户出站规则的服务端实现：规则定义存在服务端配置表（键 rule.<name>），
// 目标判定也只在服务端做。写路径是"先校验 → 落库 → 换内存快照"，读路径只读快照，
// 因此改规则对已建立的会话同样即时生效（判定发生在每条 Forward 上）。

import (
	"context"
	"sync"

	"github.com/hhy50/vnray/internal/logging"
	"github.com/hhy50/vnray/internal/rule"
	"github.com/hhy50/vnray/internal/store"
)

// ruleSet 是用户出站规则的运行时持有者，实现 admin.RuleSettings。
type ruleSet struct {
	compiled sync.Map
	settings store.Store
}

// loadRuleSet 只读取已初始化的规则；单条规则无效时跳过。
func loadRuleSet(ctx context.Context, st store.Store) *ruleSet {
	rs := &ruleSet{settings: st}
	kv, err := st.GetServiceSettings(ctx)
	if err != nil {
		logging.Warnf("user rules: load failed, using built-in defaults: %v", err)
		for name, r := range rule.Defaults() {
			if c, cerr := rule.Compile(name, r); cerr == nil {
				rs.compiled.Store(name, c)
			}
		}
		return rs
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
		rs.compiled.Store(name, c)
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
	value, ok := s.compiled.Load(name)
	if !ok {
		return rule.Pass, name
	}
	return value.(*rule.Compiled).Verdict(host), name
}

// List 返回全部规则（按名字升序）。
func (s *ruleSet) List() []rule.Named {
	if s == nil {
		return nil
	}
	out := make([]rule.Named, 0)
	s.compiled.Range(func(key, value any) bool {
		out = append(out, rule.Named{Name: key.(string), Rule: value.(*rule.Compiled).Rule()})
		return true
	})
	rule.SortNamed(out)
	return out
}

// Exists 报告规则名是否已定义。
func (s *ruleSet) Exists(name string) bool {
	if s == nil {
		return false
	}
	_, ok := s.compiled.Load(name)
	return ok
}

// SaveRule 校验、落库并换内存：校验失败不落库，落库失败不换内存。
func (s *ruleSet) SaveRule(ctx context.Context, name string, r rule.Rule) error {
	c, err := rule.Compile(name, r)
	if err != nil {
		return err
	}
	raw, err := rule.Encode(r)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, applyTimeout)
	defer cancel()
	if err := s.settings.SetServiceSettings(ctx, map[string]string{rule.Key(name): raw}); err != nil {
		return err
	}
	s.compiled.Store(name, c)
	return nil
}

// DeleteRule 删除一条规则（含 default）：引用它的用户会退回"不干预"，重启不会补回来。
func (s *ruleSet) DeleteRule(ctx context.Context, name string) error {
	if err := rule.ValidName(name); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, applyTimeout)
	defer cancel()
	if err := s.settings.SetServiceSettings(ctx, map[string]string{rule.Key(name): ""}); err != nil {
		return err
	}
	s.compiled.Delete(name)
	return nil
}

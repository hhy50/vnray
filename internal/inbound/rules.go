package inbound

import (
	"context"
	"errors"
	"fmt"

	"github.com/hhy50/vnray/internal/rule"
	"github.com/hhy50/vnray/internal/store"
)

// RuleEvaluator 按规则名判定目标，由服务端装配层注入。
type RuleEvaluator interface {
	Verdict(name, host string) (rule.Verdict, string)
}

type userPolicy struct {
	users UserStore
	rules RuleEvaluator
	proto store.Protocol
	key   string
}

func (p userPolicy) check(ctx context.Context, host string) (bool, error) {
	if p.rules == nil {
		return false, nil
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if p.users == nil {
		return false, errors.New("user rules require user configuration")
	}
	u, ok := p.users.LookupUser(ctx, p.proto, p.key)
	if !ok || u.Status != store.UserEnabled {
		return false, errors.New("user access unavailable")
	}
	verdict, name := p.rules.Verdict(u.Config.Rule, host)
	switch verdict {
	case rule.Reject:
		return false, fmt.Errorf("destination %s rejected by rule %s", host, name)
	case rule.Direct:
		return true, nil
	default:
		return false, nil
	}
}

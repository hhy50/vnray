package admin

// rules.go 是用户出站规则的管理面接口：规则定义由服务端保存（配置表键
// rule.<name>），判定也在服务端做；这里只做增删改查。

import (
	"net/http"
	"strings"

	"github.com/hhy50/vnray/internal/logging"
	"github.com/hhy50/vnray/internal/rule"
)

// RuleSettings 是用户出站规则的读写接口，由服务端装配层实现（core.ruleSet），
// 经 AdminOptions.Rules 注入。admin 只依赖接口，不反向依赖 core。
type RuleSettings interface {
	// List 返回全部规则（按名字升序）。
	List() []rule.Named
	// Exists 报告规则名是否已定义。
	Exists(name string) bool
	// SaveRule 校验并落库一条规则，成功后对数据面即时生效。
	SaveRule(name string, r rule.Rule) error
	// DeleteRule 删除一条自定义规则。
	DeleteRule(name string) error
}

// apiRuleDTO 是管理台的规则视图。
type apiRuleDTO struct {
	Name   string   `json:"name"`
	Reject []string `json:"reject"`
	Direct []string `json:"direct"`
}

// apiRules 返回全部出站规则。
func (s *AdminService) apiRules(w http.ResponseWriter, r *http.Request) {
	if s.opts.Rules == nil {
		apiFailKey(w, r, http.StatusNotFound, msgRulesNotConfigured)
		return
	}
	list := s.opts.Rules.List()
	out := make([]apiRuleDTO, 0, len(list))
	for _, item := range list {
		out = append(out, apiRuleDTO{
			Name:   item.Name,
			Reject: nonNil(item.Rule.Reject),
			Direct: nonNil(item.Rule.Direct),
		})
	}
	apiOK(w, map[string]any{"rules": out, "total": len(out)})
}

type apiRuleSaveIn struct {
	Reject []string `json:"reject"`
	Direct []string `json:"direct"`
}

// apiRuleSave 新增或覆盖一条规则（PUT /api/rules/{name}）。
func (s *AdminService) apiRuleSave(w http.ResponseWriter, r *http.Request) {
	if s.opts.Rules == nil {
		apiFailKey(w, r, http.StatusNotFound, msgRulesNotConfigured)
		return
	}
	name := strings.TrimSpace(r.PathValue("name"))
	if err := rule.ValidName(name); err != nil {
		apiFailKey(w, r, http.StatusBadRequest, msgRuleNameInvalid, err)
		return
	}
	var in apiRuleSaveIn
	if !apiDecode(w, r, &in) {
		return
	}
	item := rule.Rule{Reject: in.Reject, Direct: in.Direct}
	if err := s.opts.Rules.SaveRule(name, item); err != nil {
		logging.Warn("admin: save rule failed", "rule", name, "err", err)
		apiFailKey(w, r, http.StatusBadRequest, msgSaveFailedDetail, err)
		return
	}
	logging.Info("admin: rule saved", "rule", name,
		"reject", len(item.Reject), "direct", len(item.Direct), "actor", actorOf(r))
	apiOK(w, map[string]any{"name": name})
}

// apiRuleDelete 删除一条规则；引用它的用户退回"不干预"。
func (s *AdminService) apiRuleDelete(w http.ResponseWriter, r *http.Request) {
	if s.opts.Rules == nil {
		apiFailKey(w, r, http.StatusNotFound, msgRulesNotConfigured)
		return
	}
	name := strings.TrimSpace(r.PathValue("name"))
	if err := rule.ValidName(name); err != nil {
		apiFailKey(w, r, http.StatusBadRequest, msgRuleNameInvalid, err)
		return
	}
	if err := s.opts.Rules.DeleteRule(name); err != nil {
		logging.Warn("admin: delete rule failed", "rule", name, "err", err)
		apiFailKey(w, r, http.StatusInternalServerError, msgDeleteStoreFail)
		return
	}
	logging.Info("admin: rule deleted", "rule", name, "actor", actorOf(r))
	apiOK(w, map[string]any{"name": name})
}

// ruleNames 返回规则名清单（用户页的规则下拉用）；未接入时返回空表。
func (s *AdminService) ruleNames() []string {
	if s.opts.Rules == nil {
		return []string{}
	}
	list := s.opts.Rules.List()
	out := make([]string, 0, len(list))
	for _, item := range list {
		out = append(out, item.Name)
	}
	return out
}

// nonNil 把 nil 切片换成空切片：前端拿到 null 时要额外判空。
func nonNil(in []string) []string {
	if in == nil {
		return []string{}
	}
	return in
}

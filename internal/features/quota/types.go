// Package quota 在运行时把 entitlement 的静态档位表落到具体操作上：
// 查当前用量、比对上限、给出结构化的拒绝原因。
//
// 与相邻两个包的分工：
//   - entitlement —— 「每档能有多少」，纯数据，无 IO
//   - pricing     —— 「一次调用收多少代币」，纯算术，无 IO
//   - quota       —— 「这个用户现在还能不能做这件事」，需要查库
package quota

import (
	"fmt"
	"time"

	"github.com/squyrrl/api/internal/features/entitlement"
)

// Reason 是 402 响应里的机器可读原因，客户端据此决定引导去充值还是去升级。
//
// 拆开的理由很实际：两种 402 的补救动作完全不同 —— 余额不足要跳充值页，
// 档位超限要跳订阅页。此前 402 只回一句中文，客户端只能靠匹配文案分流，
// 那既脆弱又让服务端背上了本该属于客户端的 i18n 责任。
type Reason string

const (
	// ReasonInsufficientCredits 代币余额不足，充值即可继续。
	ReasonInsufficientCredits Reason = "insufficient_credits"
	// ReasonPlanLimit 当前档位的容量或能力不允许，需要升级订阅。
	ReasonPlanLimit Reason = "plan_limit"
	// ReasonSyncRequired 免费档不同步，该操作需要 Basic 及以上。
	// 单独一个原因而不是并进 plan_limit：它对应的引导文案完全不同
	// （「升级以启用云同步」而非「你的页面数量已达上限」），
	// 而且它是免费用户最常撞到的一条，值得让客户端单独处理。
	ReasonSyncRequired Reason = "sync_required"
	// ReasonDataRestricted 这条数据超出了当前档位，正处在降级后的生命周期里
	// （宽限期不可修改 / 冻结期不可读取）。
	//
	// 与 ReasonPlanLimit 分开：那一条说的是「你不能再加了」，动作是少建几条或升级；
	// 这一条说的是「这件已经存在的东西你暂时动不了」，而且**带截止日期**。
	// 客户端要显示的东西完全不同，合并成一个 reason 就只能靠猜。
	ReasonDataRestricted Reason = "data_restricted"
)

// Limit 标识撞到的是哪一项门槛。取值与 entitlement.Tier 的字段一一对应。
type Limit string

const (
	LimitSnippets    Limit = "snippets"
	LimitPages       Limit = "pages"
	LimitTags        Limit = "tags"
	LimitDevices     Limit = "devices"
	LimitBindings    Limit = "bindings"
	LimitHiddenPages Limit = "hidden_pages"
)

// Error 是所有 402 的统一载体。handler 把它序列化成响应体。
//
// 字段按 Reason 分组填写，未用到的留空并由 omitempty 省略 —— 一个响应体里
// 同时出现 balance 和 cap 会让客户端不知道该看哪个。
type Error struct {
	Reason Reason `json:"reason"`
	Msg    string `json:"error"`

	// —— ReasonPlanLimit / ReasonSyncRequired ——
	Limit   Limit  `json:"limit,omitempty"`
	Cap     *int   `json:"cap,omitempty"`
	Current *int   `json:"current,omitempty"`
	Plan    string `json:"plan,omitempty"`
	// RequiredPlan 是能满足本次操作的最低档位。
	// 为空表示**最高档也不够** —— 客户端必须区分这两种情况，
	// 否则会把「你已经是至尊版了，这条碎片确实放不下」显示成「请升级」。
	RequiredPlan string `json:"required_plan,omitempty"`

	// —— ReasonInsufficientCredits ——
	Balance  *int64 `json:"balance,omitempty"`
	Required *int64 `json:"required,omitempty"`

	// —— ReasonDataRestricted ——
	// RestrictStage 是 "grace"（可读可删不可改）或 "frozen"（列表可见、详情不可读）。
	RestrictStage string `json:"restrict_stage,omitempty"`
	// RestrictUntil 是当前阶段的结束时刻。冻结期结束即永久删除，所以这个字段
	// 承载的是一个真实的倒计时，而不是一句软性提示。
	RestrictUntil *time.Time `json:"restrict_until,omitempty"`
}

func (e *Error) Error() string { return e.Msg }

// IsQuotaError 供 handler 判定是否该回 402。
func IsQuotaError(err error) (*Error, bool) {
	qe, ok := err.(*Error)
	return qe, ok
}

// ErrDataRestricted 组装降级后受限数据的 402。
//
// stage 是 "grace" / "frozen"，until 是该阶段的结束时刻 —— 客户端要靠它显示
// 「还有 N 天」。不给截止时间的话，这条提示就只是一句无从行动的坏消息。
func ErrDataRestricted(msg, stage string, until time.Time, plan string) *Error {
	return &Error{
		Reason:        ReasonDataRestricted,
		Msg:           msg,
		Plan:          plan,
		RestrictStage: stage,
		RestrictUntil: &until,
		RequiredPlan:  "",
	}
}

// newLimitError 组装一个「档位容量不足」的错误，并算出需要升到哪一档。
func newLimitError(limit Limit, plan string, cap, current int) *Error {
	req := minTierFor(limit, current+1)
	return &Error{
		Reason:       ReasonPlanLimit,
		Msg:          fmt.Sprintf("%s limit reached on plan %s (%d/%d)", limit, plan, current, cap),
		Limit:        limit,
		Cap:          &cap,
		Current:      &current,
		Plan:         plan,
		RequiredPlan: req,
	}
}

// newCapabilityError 组装一个「档位不具备该能力」的错误（布尔门槛，无数量概念）。
func newCapabilityError(limit Limit, plan, requiredPlan string) *Error {
	return &Error{
		Reason:       ReasonPlanLimit,
		Msg:          fmt.Sprintf("%s not available on plan %s", limit, plan),
		Limit:        limit,
		Plan:         plan,
		RequiredPlan: requiredPlan,
	}
}

// ErrSyncRequired 是免费档尝试写服务端数据时的拒绝。
func ErrSyncRequired(plan string) *Error {
	return &Error{
		Reason:       ReasonSyncRequired,
		Msg:          fmt.Sprintf("cloud sync requires a paid plan (current: %s)", plan),
		Plan:         plan,
		RequiredPlan: entitlement.Basic,
	}
}

// ErrInsufficientCredits 由消费路径构造，供 handler 统一走 402。
func ErrInsufficientCredits(balance, required int64) *Error {
	return &Error{
		Reason:   ReasonInsufficientCredits,
		Msg:      fmt.Sprintf("insufficient credits: have %d, need %d", balance, required),
		Balance:  &balance,
		Required: &required,
	}
}

// minTierFor 找出能容纳 need 的最低档位；没有任何档位够用时返回空串。
//
// 返回空串是一个必须被 handler 区分的状态：它意味着「升级也没用」。
// 把它和「需要升到 standard」混为一谈，会让至尊版用户看到一个点不动的升级按钮。
func minTierFor(limit Limit, need int) string {
	for _, key := range entitlement.Order {
		t, ok := entitlement.Of(key)
		if !ok {
			continue
		}
		if capOf(t, limit) >= need {
			return key
		}
	}
	return ""
}

func capOf(t entitlement.Tier, limit Limit) int {
	switch limit {
	case LimitSnippets:
		return t.Snippets
	case LimitPages:
		return t.Pages
	case LimitTags:
		return t.Tags
	case LimitDevices:
		return t.Devices
	case LimitBindings:
		return t.BindingsPerPlatform
	default:
		return 0
	}
}

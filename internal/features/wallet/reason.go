package wallet

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
)

// AdminReasonCategories 是后台手工调账允许的 reason 类别，管理后台的下拉框与之一一对应。
//
// reason 是 credits_ledger 里唯一能解释一笔人工流水的字段，而表里不记操作人。
// 放任自由文本的结果是「admin_grant」「补偿」「test」混在一起，事后既筛不出全部人工操作，
// 也按不了类别汇总。固定前缀 admin_ 让人工流水与系统写入的 purchase:* / parse_uri:*
// 一条 LIKE 就能分开；固定类别让按用途统计成为可能；冒号后的说明才是给人看的。
var AdminReasonCategories = []string{
	"admin_grant",      // 赠送：活动、合作、客服关怀
	"admin_comp",       // 故障补偿
	"admin_refund",     // 退回误扣：重复扣费、解析失败
	"admin_revoke",     // 收回：误发、违规
	"admin_correction", // 冲正：抵消一笔录错的人工流水
	"admin_test",       // 测试账号
}

// MaxAdminReasonDetail 限制说明的字符数（按 rune 计，中文不吃亏）。
// 够写工单号 + 一句来由；更长的内容应该在工单里，这里放能对上它的编号。
const MaxAdminReasonDetail = 200

// ErrInvalidAdminReason 由 normalizeAdminReason 在格式不符时返回，handler 映射到 400。
var ErrInvalidAdminReason = errors.New("invalid admin reason")

// normalizeAdminReason 校验并规范化后台调账的 reason，格式为 `<类别>:<说明>`。
// 返回去掉首尾空白后的 `<类别>:<说明>`。
//
// 只用于内部调账端点：purchase:* / parse_uri:* 由服务端自己拼，不经过这里。
func normalizeAdminReason(reason string) (string, error) {
	category, detail, ok := strings.Cut(strings.TrimSpace(reason), ":")
	if !ok {
		return "", fmt.Errorf("%w: want <category>:<detail>, got %q", ErrInvalidAdminReason, reason)
	}
	if !slices.Contains(AdminReasonCategories, category) {
		return "", fmt.Errorf("%w: unknown category %q, want one of %s",
			ErrInvalidAdminReason, category, strings.Join(AdminReasonCategories, " / "))
	}
	detail = strings.TrimSpace(detail)
	if detail == "" {
		return "", fmt.Errorf("%w: detail after %q is empty", ErrInvalidAdminReason, category+":")
	}
	if n := utf8.RuneCountInString(detail); n > MaxAdminReasonDetail {
		return "", fmt.Errorf("%w: detail is %d chars, max %d", ErrInvalidAdminReason, n, MaxAdminReasonDetail)
	}
	// 换行、制表符等会让按行导出的对账表错位，一律拒绝。
	if strings.ContainsFunc(detail, unicode.IsControl) {
		return "", fmt.Errorf("%w: detail contains control characters", ErrInvalidAdminReason)
	}
	return category + ":" + detail, nil
}

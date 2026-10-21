package parser

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"unicode"

	"github.com/google/uuid"

	"github.com/squyrrl/api/internal/features/extapi"
)

// ShareCoder 是可选接口：平台有一种没有链接的分享 —— 口令。
//
// 抖音文章的分享面板只有「复制口令」，复制出来的是
//
//	8k A@T.Yz :9pm 10/17 【标题】长按复制打开抖音，即可阅读文章 ︽︽cz9zRIbcJa9︽︽
//
// 里面没有链接，作品 ID 只有抖音自己能从码里换出来。所以和短链不同，换这一步要花钱：
// 调一个换口令的付费接口（Adapter.Resolve）拿到作品链接，再走正常解析，缓存键仍是作品 ID。
// 每次分享的口令都不一样，同一作品被不同人分享，这一步各付各的；多出的成本由平台的倍价承担。
type ShareCoder interface {
	ShareCodePatterns() []*regexp.Regexp // 第 1 个捕获组是口令里的码
}

// FindShareCode 返回认领这段口令的平台与口令里的码。
func (r *Registry) FindShareCode(text string) (Parser, string, bool) {
	for _, p := range r.parsers {
		sc, ok := p.(ShareCoder)
		if !ok {
			continue
		}
		for _, re := range sc.ShareCodePatterns() {
			if m := re.FindStringSubmatch(text); m != nil && m[1] != "" {
				return p, m[1], true
			}
		}
	}
	return nil, "", false
}

var (
	errNoResolver = errors.New("平台没有可用的换口令接口")
	errShareCode  = errors.New("口令换作品失败")
)

// resolveShareCode 用平台的换口令接口把口令换成作品链接，每次调用记一个 Attempt。
// 平台没有能用的换口令接口时返回 errNoResolver（客户端拿的是旧清单，不收费也不记录）。
func (s *Service) resolveShareCode(ctx context.Context, p Parser, eps []extapi.Endpoint, t Target) (string, []Attempt, error) {
	var attempts []Attempt
	spent := map[uuid.UUID]int64{}
	for _, ep := range eps {
		ad, ok := s.adapters.callable(ep)
		if !ok || ad.Resolve == nil {
			continue
		}
		ep.BalanceMicros -= spent[ep.AccountID]
		epID, accID := ep.ID, ep.AccountID
		a := Attempt{Seq: len(attempts) + 1, Source: ep.Slug, EndpointID: &epID, AccountID: &accID, AccountName: ep.AccountName}
		if ep.UnitPriceMicros > 0 && ep.BalanceMicros <= 0 {
			a.Outcome, a.ErrorKind, a.Error = AttemptSkipped, extapi.KindNoBalance, "账户余额已用完"
			attempts = append(attempts, a)
			continue
		}

		call := s.extapi.Call(ctx, ep, ad.Request(t), "口令:"+t.Code)
		spent[ep.AccountID] += call.CostMicros
		a.HTTPStatus, a.LatencyMs, a.CostMicros, a.SampleID = call.HTTPStatus, call.LatencyMs, call.CostMicros, call.SampleID
		if call.CostMicros > 0 {
			bal := call.BalanceAfter
			a.BalanceAfter = &bal
		}
		if call.Err != nil {
			a.Outcome, a.ErrorKind, a.Error = AttemptFailed, call.Err.Kind, call.Err.Msg
			attempts = append(attempts, a)
			continue
		}
		link, err := ad.Resolve(call.Data, t)
		if err == nil {
			if _, ok := p.Match(link); !ok {
				err = fmt.Errorf("换来的链接 %s 不归 %s", link, p.Provider())
			}
		}
		if err != nil {
			a.Outcome, a.ErrorKind, a.Error = AttemptFailed, extapi.KindDecode, err.Error()
			attempts = append(attempts, a)
			continue
		}
		a.Outcome = AttemptOK
		return link, append(attempts, a), nil
	}
	if len(attempts) == 0 {
		return "", nil, errNoResolver
	}
	return "", attempts, errShareCode
}

func attemptsCost(as []Attempt) int64 {
	var n int64
	for _, a := range as {
		n += a.CostMicros
	}
	return n
}

// joinAttempts 把兜底链的尝试接在换口令的尝试后面，序号顺延。复制一份：
// 兜底链的结果在并发合并时与别的请求共用。
func joinAttempts(pre, rest []Attempt) []Attempt {
	out := make([]Attempt, 0, len(pre)+len(rest))
	out = append(out, pre...)
	for _, a := range rest {
		a.Seq += len(pre)
		out = append(out, a)
	}
	return out
}

// deductSpent 从接口的余额快照里扣掉换口令花的钱：同一账户下的接口共用余额，
// 否则兜底链的「余额耗尽就跳过」会慢一拍（同 runChain 里的 spent）。
func deductSpent(eps []extapi.Endpoint, attempts []Attempt) []extapi.Endpoint {
	spent := map[uuid.UUID]int64{}
	for _, a := range attempts {
		if a.AccountID != nil {
			spent[*a.AccountID] += a.CostMicros
		}
	}
	out := make([]extapi.Endpoint, len(eps))
	for i, ep := range eps {
		ep.BalanceMicros -= spent[ep.AccountID]
		out[i] = ep
	}
	return out
}

// douyinShareText 把口令整理成换口令接口认得的样子：码两边换成 ︽︽。
//
// 抖音每次分享给码配的符号不同（︽︽…︽︽、➝➝…※※、ŠŠ…ŠŠ），而 TikHub 只认它示例里的 ︽︽：
// 原样送 ŠŠ 的口令失败，只把两边换成 ︽︽、其余不动就成功了（实测 2026-10-21）。所以只换符号，
// 码前面的文字照原样送。码前面要去掉的是紧挨着的那串符号：汉字、空白、ASCII 字母数字留下
// （Š 是拉丁字母，不能按「是不是字母」判断）。
func douyinShareText(text, code string) string {
	i := strings.LastIndex(text, code)
	if i < 0 {
		return text
	}
	prefix := strings.TrimRightFunc(text[:i], func(r rune) bool {
		return !unicode.IsSpace(r) && !unicode.Is(unicode.Han, r) && !isASCIIAlnum(r)
	})
	return prefix + "︽︽" + code + "︽︽"
}

func isASCIIAlnum(r rune) bool {
	return r < 128 && (unicode.IsLetter(r) || unicode.IsDigit(r))
}

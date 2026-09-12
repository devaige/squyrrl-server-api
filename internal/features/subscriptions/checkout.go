package subscriptions

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

var (
	// ErrCheckoutUnconfigured 缺密钥或价目映射 —— 购买入口整体不可用。
	ErrCheckoutUnconfigured = errors.New("stripe checkout 未配置")
	// ErrUnknownSKU 请求的商品不在价目映射里。
	ErrUnknownSKU = errors.New("该商品暂不可购买")
)

// PriceBook 是 SKU → Stripe price id 的映射，启动时从环境变量解析一次。
//
// 解析失败**不 panic 也不静默**：把它降级为「购买入口不可用」，其余功能照常。
// 一个填错的价目表不该让整个 API 起不来 —— 那会把一个收款问题放大成一次全站故障。
type PriceBook struct {
	prices map[string]string
}

// NewPriceBook 解析 JSON 映射。空串返回一个空价目表（购买入口不可用，但不报错）。
func NewPriceBook(raw string) (*PriceBook, error) {
	pb := &PriceBook{prices: map[string]string{}}
	if strings.TrimSpace(raw) == "" {
		return pb, nil
	}
	if err := json.Unmarshal([]byte(raw), &pb.prices); err != nil {
		return pb, fmt.Errorf("解析 SQUYRRL_STRIPE_PRICES: %w", err)
	}
	return pb, nil
}

// SKUKey 拼出价目表的键。代币加购没有周期，统一用 `once`。
func SKUKey(kind, tier, period string) string {
	return kind + ":" + tier + ":" + period
}

func (p *PriceBook) lookup(kind, tier, period string) (string, bool) {
	id, ok := p.prices[SKUKey(kind, tier, period)]
	return id, ok
}

// CheckoutRequest 是客户端发起购买时提交的 SKU。
type CheckoutRequest struct {
	// Kind: plan | storage | credits
	Kind string `json:"kind" binding:"required,oneof=plan storage credits"`
	// Tier: 档位键（basic…maximum）、存储档位键（s20…）或代币包键（p1/p5/p20）
	Tier string `json:"tier" binding:"required"`
	// Period: monthly | yearly | once
	Period string `json:"period" binding:"required,oneof=monthly yearly once"`
}

// Checkout 调 Stripe API 创建一个 Checkout Session，返回它的支付页 URL。
//
// 手写 net/http 而不是引 stripe-go：这个包已经手写了 webhook 验签，整套用到的
// Stripe 接口就是「创建 session」这一个 POST。为一个表单提交引入一个会自带
// 上百个模型的 SDK，收益是负的（与 ADR-060 用 net/http 调 Resend 同一判断）。
type Checkout struct {
	secretKey  string
	prices     *PriceBook
	returnBase string
	http       *http.Client
}

func NewCheckout(secretKey string, prices *PriceBook, returnBase string) *Checkout {
	return &Checkout{
		secretKey:  secretKey,
		prices:     prices,
		returnBase: strings.TrimRight(returnBase, "/"),
		http:       &http.Client{Timeout: 15 * time.Second},
	}
}

// Enabled 报告购买入口是否可用。两项缺一不可，与边缘直传的 fail-closed 同理：
// 只有密钥没有价目表时若判为可用，用户会走到一个「商品不存在」的死胡同。
func (c *Checkout) Enabled() bool {
	return c.secretKey != "" && len(c.prices.prices) > 0
}

// Create 为某个用户的某个 SKU 创建结账会话。
//
// storageGB / credits 由服务端从自己的价目表推导后写进 metadata，**不接受客户端传入**：
// webhook 完全信任 metadata（它是唯一能说明「买了什么」的地方），
// 让客户端往里塞数字，等于让客户端自己决定买 5 GB 还是 5 TB。
func (c *Checkout) Create(
	ctx context.Context, userID uuid.UUID, req CheckoutRequest, storageGB int, credits int64,
) (string, error) {
	if !c.Enabled() {
		return "", ErrCheckoutUnconfigured
	}
	priceID, ok := c.prices.lookup(req.Kind, req.Tier, req.Period)
	if !ok {
		return "", ErrUnknownSKU
	}

	mode := "subscription"
	if req.Kind == "credits" {
		mode = "payment"
	}

	form := url.Values{}
	form.Set("mode", mode)
	form.Set("line_items[0][price]", priceID)
	form.Set("line_items[0][quantity]", "1")
	form.Set("success_url", c.returnBase+"/checkout/done?session_id={CHECKOUT_SESSION_ID}")
	form.Set("cancel_url", c.returnBase+"/checkout/cancelled")
	form.Set("client_reference_id", userID.String())

	meta := map[string]string{
		"squyrrl_user_id": userID.String(),
		"squyrrl_kind":    req.Kind,
		"squyrrl_tier":    req.Tier,
		"squyrrl_period":  req.Period,
	}
	if req.Kind == "storage" {
		meta["squyrrl_storage_gb"] = strconv.Itoa(storageGB)
	}
	if req.Kind == "credits" {
		meta["squyrrl_credits"] = strconv.FormatInt(credits, 10)
	}
	for k, v := range meta {
		form.Set("metadata["+k+"]", v)
		// 订阅模式下 session 的 metadata **不会**自动复制到 subscription 对象上，
		// 而我们的 webhook 读的正是 subscription.metadata。漏掉这一行的表现是：
		// 用户付款成功，随后每一个续期事件都因为「认不出用户」而被丢弃。
		if mode == "subscription" {
			form.Set("subscription_data[metadata]["+k+"]", v)
		}
	}

	body, err := c.post(ctx, "https://api.stripe.com/v1/checkout/sessions", form)
	if err != nil {
		return "", err
	}
	var out struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal(body, &out); err != nil || out.URL == "" {
		return "", fmt.Errorf("stripe 未返回结账 URL: %s", truncate(body, 200))
	}
	return out.URL, nil
}

func (c *Checkout) post(ctx context.Context, endpoint string, form url.Values) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint,
		strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.SetBasicAuth(c.secretKey, "")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 300 {
		// 回传 Stripe 的原文但截断：它的错误信息对排查极有价值，
		// 而完整回包可能很长且含无关字段。
		return nil, fmt.Errorf("stripe %d: %s", resp.StatusCode, truncate(body, 300))
	}
	return body, nil
}

func truncate(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "…"
}

package auth

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"
)

const resendEndpoint = "https://api.resend.com/emails"

// Mailer 通过 Resend HTTP API 发送邮件。
//
// apiKey 为空时沿用日志模式，不会投递邮件；正式部署必须配置 Resend key。
type Mailer struct {
	apiKey string
	from   string // 完整 From 头，例如 "Squyrrl <noreply@squyrrl.com>"；须为 Resend 已验证发信域
	client *http.Client
}

func NewMailer(apiKey, from string) *Mailer {
	return &Mailer{
		apiKey: apiKey,
		from:   from,
		client: &http.Client{Timeout: 10 * time.Second},
	}
}

// Enabled 报告是否会真的外发邮件。发信预算只在「真会消耗配额」时才检查 ——
// 无 key 时 SendOTP 不外发，不消耗发信预算。
func (m *Mailer) Enabled() bool { return m.apiKey != "" }

// SendOTP 发送登录 OTP 邮件
func (m *Mailer) SendOTP(to, code string) error {
	subject := "Squyrrl 登录验证码"
	body := fmt.Sprintf(
		"您的 Squyrrl 登录验证码：%s\n\n"+
			"该验证码 10 分钟内有效。如非本人操作请忽略本邮件。\n",
		code,
	)

	// 未配置 key：本地开发退化为日志输出（含明文 code），不外发。仅在无 key 时发生，
	// 生产必配 SQUYRRL_RESEND_API_KEY，不会走到这里。
	if m.apiKey == "" {
		slog.Warn("RESEND_API_KEY 未配置，OTP 改为日志输出（仅限本地开发）", "to", to, "code", code)
		return nil
	}

	return m.send(to, subject, body)
}

func (m *Mailer) send(to, subject, text string) error {
	payload, err := json.Marshal(map[string]any{
		"from":    m.from,
		"to":      []string{to},
		"subject": subject,
		"text":    text,
	})
	if err != nil {
		return err
	}

	req, err := http.NewRequest(http.MethodPost, resendEndpoint, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+m.apiKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := m.client.Do(req)
	if err != nil {
		slog.Error("OTP 邮件发送失败", "to", to, "err", err)
		return err
	}
	defer resp.Body.Close()

	// Resend 成功返回 2xx（body 携带 {"id":...}）；非 2xx 回读 body（限长）便于排错
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		slog.Error("OTP 邮件发送失败", "to", to, "status", resp.StatusCode, "body", string(msg))
		return fmt.Errorf("resend 返回 %d: %s", resp.StatusCode, msg)
	}

	slog.Info("OTP 邮件已发送", "to", to)
	return nil
}

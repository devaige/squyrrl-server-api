package auth

import (
	"fmt"
	"log/slog"
	"mime"
	"net/mail"
	"net/smtp"
	"strings"
)

// Mailer 用 net/smtp 发邮件。dev 环境对接 Mailhog，无 auth、无 TLS。
type Mailer struct {
	addr        string // host:port
	auth        smtp.Auth
	from        string // 完整 From 头，例如 "Squyrrl <noreply@squyrrl.app>"
	fromAddress string // 仅地址部分，作为 SMTP 信封 MAIL FROM
}

func NewMailer(host string, port int, user, pass, from string) *Mailer {
	var auth smtp.Auth
	if user != "" {
		auth = smtp.PlainAuth("", user, pass, host)
	}

	fromAddr := from
	if parsed, err := mail.ParseAddress(from); err == nil {
		fromAddr = parsed.Address
	}

	return &Mailer{
		addr:        fmt.Sprintf("%s:%d", host, port),
		auth:        auth,
		from:        from,
		fromAddress: fromAddr,
	}
}

// SendOTP 发送登录 OTP 邮件
func (m *Mailer) SendOTP(to, code string) error {
	subject := "Squyrrl 登录验证码"
	body := fmt.Sprintf(
		"您的 Squyrrl 登录验证码：%s\n\n"+
			"该验证码 10 分钟内有效。如非本人操作请忽略本邮件。\n",
		code,
	)
	msg := buildMessage(m.from, to, subject, body)

	if err := smtp.SendMail(m.addr, m.auth, m.fromAddress, []string{to}, msg); err != nil {
		slog.Error("OTP 邮件发送失败", "to", to, "err", err)
		return err
	}
	slog.Info("OTP 邮件已发送", "to", to)
	return nil
}

// buildMessage 拼装一封 UTF-8 文本邮件，主题做 RFC 2047 编码以容纳中文
func buildMessage(from, to, subject, body string) []byte {
	var sb strings.Builder
	sb.WriteString("From: " + from + "\r\n")
	sb.WriteString("To: " + to + "\r\n")
	sb.WriteString("Subject: " + mime.QEncoding.Encode("UTF-8", subject) + "\r\n")
	sb.WriteString("MIME-Version: 1.0\r\n")
	sb.WriteString("Content-Type: text/plain; charset=UTF-8\r\n")
	sb.WriteString("Content-Transfer-Encoding: 8bit\r\n")
	sb.WriteString("\r\n")
	sb.WriteString(body)
	return []byte(sb.String())
}

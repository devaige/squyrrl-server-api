package auth

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/google/uuid"
)

// 业务常量。Phase 1 固定值，后续按订阅档位动态化
const (
	otpTTL          = 10 * time.Minute
	accessTokenTTL  = 1 * time.Hour
	refreshTokenTTL = 30 * 24 * time.Hour
)

// OTPGuard 是 OTP 发信的防滥用参数（三层里的第 ① ③ 层；第 ② 层按 IP 限流在中间件里）。
//
// 三层各挡一类，缺一层漏一类：
//
//	① Cooldown    —— 同一邮箱狂点
//	② 按 IP 限流   —— 枚举不同邮箱（① 对这类完全无效）
//	③ DailyBudget —— 前两层都被绕过时，保住发信配额本身
//
// 为什么必须由服务端承担：Cloudflare 免费版的限流规则周期与封禁时长都固定 10 秒，
// 最严的 1 次/10 秒也等于 8,640 次/天，而 Resend 免费版是 100 封/天 —— 差 86 倍。
// 那条边缘规则挡的是「客户端重试循环写错」这类手滑，不是攻击。详见 ADR-074。
//
// MaxAttempts 守的是**另一条路**：上面三层都在发信侧，而验证侧此前完全没有闸门。
// 6 位码 + 10 分钟 TTL = 10^6 的空间，猜错除了 401 没有任何代价；免费版 Cloudflare
// 全 zone 只有一条限流规则且已经给了发信端点，所以边缘也帮不上忙。
// 计数器把攻击成本从「速率」换成「必须重新触发发信」，于是它回落到上面三层的射程内。
type OTPGuard struct {
	Cooldown    time.Duration // <=0 关闭
	DailyBudget int           // <=0 关闭；按滚动 24 小时计
	MaxAttempts int           // <=0 关闭；单个邮箱在一个码周期内允许猜错的次数
}

type Service struct {
	repo   *Repo
	mailer *Mailer
	guard  OTPGuard
}

func NewService(repo *Repo, mailer *Mailer, guard OTPGuard) *Service {
	return &Service{repo: repo, mailer: mailer, guard: guard}
}

// RequestEmailOTP 生成 OTP 落库并发邮件
// 不区分"邮箱不存在"错误，永远返回 nil 给上游，避免泄漏邮箱存在性
//
// 两道闸门都以 nil 返回（与正常路径无法区分），因为这个端点的返回值本来就刻意
// 不携带任何状态 —— 一旦让「被冷却」和「已发送」看起来不同，它就重新变成一个
// 邮箱存在性与限流状态的探测器。
func (s *Service) RequestEmailOTP(ctx context.Context, email string) error {
	// ① 按邮箱冷却。窗口内已有未消费的 OTP 就什么都不做 —— **不是重发那一封**：
	// 库里只存 SHA-256，明文取不回来。用户手上那封在 otpTTL(10min) 内仍然有效，
	// 所以「不做任何事」对真实用户是无损的，对刷子则是零成本。
	if s.guard.Cooldown > 0 {
		recent, err := s.repo.HasRecentOTP(ctx, email, "login", s.guard.Cooldown)
		if err != nil {
			return err
		}
		if recent {
			return nil
		}
	}

	// ③ 全局发信预算。只在真会消耗配额时检查（dev 无 key 时不计）。
	// 放在冷却之后：冷却是一次索引命中，能挡掉的请求就不必再做一次全表 count。
	if s.guard.DailyBudget > 0 && s.mailer.Enabled() {
		sent, err := s.repo.CountOTPsSince(ctx, 24*time.Hour)
		if err != nil {
			return err
		}
		if sent >= s.guard.DailyBudget {
			// 熔断时**既不建行也不发信**：建了行不发信只会让表继续膨胀，
			// 而这张表的规模正是靠「超预算就不建行」自我限幅的。
			// 这里必须吵 —— 熔断意味着真实用户此刻也登不进来，是需要人介入的状态，
			// 而对外仍返回 nil，不给攻击者「打穿了」的信号。
			slog.Error("OTP 发信预算已耗尽，本次不发信也不建 OTP，真实用户将无法登录",
				"sent_24h", sent, "budget", s.guard.DailyBudget)
			return nil
		}
	}

	code, err := newOTPCode()
	if err != nil {
		return err
	}
	if err := s.repo.CreateEmailOTP(ctx, email, hashSHA256(code), "login", time.Now().Add(otpTTL)); err != nil {
		return err
	}
	if err := s.mailer.SendOTP(email, code); err != nil {
		// 邮件失败不影响 API 返回；OTP 仍可被消费，但用户未必能拿到
		slog.Error("OTP 已入库但邮件发送失败", "email", email, "err", err)
	}
	return nil
}

// VerifyEmailOTP 校验 OTP，注册或登录用户，签发会话
func (s *Service) VerifyEmailOTP(ctx context.Context, email, code, deviceName, platform string) (*LoginResult, error) {
	if err := s.repo.ConsumeEmailOTP(ctx, email, hashSHA256(code), "login"); err != nil {
		if errors.Is(err, ErrNotFound) {
			s.penalizeFailedOTP(ctx, email, "login")
			return nil, ErrInvalidCredentials
		}
		return nil, err
	}

	user, err := s.repo.UpsertUserByEmail(ctx, email)
	if err != nil {
		return nil, err
	}
	device, err := s.repo.UpsertDevice(ctx, user.ID, deviceName, platform)
	if err != nil {
		return nil, err
	}

	return s.issueSession(ctx, user, device)
}

// penalizeFailedOTP 给一次失败的验证记账，用尽次数即销毁该邮箱下所有存活的码。
//
// **销毁而不是锁定邮箱**：锁定是一个更好用的拒绝服务武器 —— 攻击者猜错几次就能把
// 真实用户挡在门外一段时间。销毁则只让那一批码作废，用户重新要一封即可；而且销毁
// 写的正是 consumed_at，冷却层（HasRecentOTP 要求 consumed_at IS NULL）随之解除，
// 用户不必再等满 Cooldown。代价是攻击者能让别人手里的码提前失效，这个骚扰上限低、
// 且不放大发信量：真正约束发信速率的是按 IP 限流与全局日预算，不是冷却窗口。
//
// 失败只记日志：此刻已经准备返回 401 了，记账出错不该把它变成 500 —— 那反而给了
// 攻击者一个「计数器挂了」的旁路信号。
func (s *Service) penalizeFailedOTP(ctx context.Context, email, purpose string) {
	if s.guard.MaxAttempts <= 0 {
		return
	}
	burned, err := s.repo.RecordFailedOTPAttempt(ctx, email, purpose, s.guard.MaxAttempts)
	if err != nil {
		slog.Error("OTP 失败计数写入失败", "email", email, "err", err)
		return
	}
	if burned > 0 {
		// 正常用户手滑很少连错到上限，连续出现即是猜解特征，留给运营侧观察。
		slog.Warn("OTP 尝试次数用尽，已销毁该邮箱下的验证码",
			"email", email, "purpose", purpose, "burned", burned, "max_attempts", s.guard.MaxAttempts)
	}
}

// IssueSessionForUser 给已认证用户签发一对新 token + 注册/复用设备
// 同时被邮箱 OTP 与 Passkey 登录流程使用
func (s *Service) IssueSessionForUser(ctx context.Context, user *User, deviceName, platform string) (*LoginResult, error) {
	device, err := s.repo.UpsertDevice(ctx, user.ID, deviceName, platform)
	if err != nil {
		return nil, err
	}
	return s.issueSession(ctx, user, device)
}

// 暴露给 passkey 流程使用 — 内部调用
func (s *Service) Repo() *Repo { return s.repo }

// issueSession 生成 token 对、写入 sessions 表
func (s *Service) issueSession(ctx context.Context, user *User, device *Device) (*LoginResult, error) {
	accessTok, err := newOpaqueToken()
	if err != nil {
		return nil, err
	}
	refreshTok, err := newOpaqueToken()
	if err != nil {
		return nil, err
	}

	now := time.Now()
	accessExp := now.Add(accessTokenTTL)
	refreshExp := now.Add(refreshTokenTTL)

	if _, err := s.repo.CreateSession(ctx, user.ID, device.ID,
		hashSHA256(accessTok), hashSHA256(refreshTok), accessExp, refreshExp); err != nil {
		return nil, err
	}

	return &LoginResult{
		User:   user,
		Device: device,
		Tokens: &TokenPair{
			AccessToken:      accessTok,
			RefreshToken:     refreshTok,
			AccessExpiresAt:  accessExp,
			RefreshExpiresAt: refreshExp,
		},
	}, nil
}

// Refresh 用 refresh token 旋转出新的 access token；refresh 不旋转
func (s *Service) Refresh(ctx context.Context, refreshToken string) (*TokenPair, error) {
	sess, err := s.repo.FindSessionByRefreshHash(ctx, hashSHA256(refreshToken))
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, ErrSessionRevoked
		}
		return nil, err
	}

	accessTok, err := newOpaqueToken()
	if err != nil {
		return nil, err
	}
	accessExp := time.Now().Add(accessTokenTTL)
	if err := s.repo.RotateAccessToken(ctx, sess.ID, hashSHA256(accessTok), accessExp); err != nil {
		return nil, err
	}

	return &TokenPair{
		AccessToken:      accessTok,
		RefreshToken:     refreshToken,
		AccessExpiresAt:  accessExp,
		RefreshExpiresAt: sess.RefreshExpiresAt,
	}, nil
}

// Logout 撤销当前会话
func (s *Service) Logout(ctx context.Context, sessionID uuid.UUID) error {
	return s.repo.RevokeSession(ctx, sessionID)
}

// ValidateAccessToken 鉴权中间件入口
func (s *Service) ValidateAccessToken(ctx context.Context, token string) (*Identity, error) {
	sess, err := s.repo.FindSessionByAccessHash(ctx, hashSHA256(token))
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, ErrSessionRevoked
		}
		return nil, err
	}
	return &Identity{
		UserID:    sess.UserID,
		DeviceID:  sess.DeviceID,
		SessionID: sess.ID,
	}, nil
}

// GetUser 用于 /me 端点
func (s *Service) GetUser(ctx context.Context, id uuid.UUID) (*User, error) {
	return s.repo.GetUser(ctx, id)
}

// IssueQRCode 已登录设备调用：拿一个 90 秒有效的绑定码
func (s *Service) IssueQRCode(ctx context.Context, userID uuid.UUID) (*QRCode, error) {
	return s.repo.IssueQRCode(ctx, userID)
}

// RedeemQRCode 新设备调用：兑换绑定码 + 设备元数据 → 完整 LoginResult
func (s *Service) RedeemQRCode(ctx context.Context, code, deviceName, platform string) (*LoginResult, error) {
	userID, err := s.repo.ConsumeQRCode(ctx, code)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, ErrInvalidCredentials
		}
		return nil, err
	}
	user, err := s.repo.GetUser(ctx, userID)
	if err != nil {
		return nil, err
	}
	return s.IssueSessionForUser(ctx, user, deviceName, platform)
}

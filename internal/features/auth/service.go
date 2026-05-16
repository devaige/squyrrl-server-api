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

type Service struct {
	repo   *Repo
	mailer *Mailer
}

func NewService(repo *Repo, mailer *Mailer) *Service {
	return &Service{repo: repo, mailer: mailer}
}

// RequestEmailOTP 生成 OTP 落库并发邮件
// 不区分"邮箱不存在"错误，永远返回 nil 给上游，避免泄漏邮箱存在性
func (s *Service) RequestEmailOTP(ctx context.Context, email string) error {
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

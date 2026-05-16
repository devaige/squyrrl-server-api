package auth

import (
	"context"
	"errors"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"
	"github.com/google/uuid"
)

// PasskeyService 编排 WebAuthn ceremony，依赖 auth.Service 复用 user/device/session 写入逻辑
type PasskeyService struct {
	web   *webauthn.WebAuthn
	store *PasskeySessionStore
	auth  *Service
}

func NewPasskeyService(w *webauthn.WebAuthn, store *PasskeySessionStore, authSvc *Service) *PasskeyService {
	return &PasskeyService{web: w, store: store, auth: authSvc}
}

// =============================================================================
// 注册（已登录用户绑定新 passkey）
// =============================================================================

// BeginRegistration 返回给客户端 navigator.credentials.create({publicKey:...}) 的参数 + session_id
func (s *PasskeyService) BeginRegistration(
	ctx context.Context,
	userID uuid.UUID,
) (*protocol.PublicKeyCredentialCreationOptions, string, error) {
	user, err := s.auth.repo.GetUser(ctx, userID)
	if err != nil {
		return nil, "", err
	}
	creds, err := s.auth.repo.ListUserCredentials(ctx, userID)
	if err != nil {
		return nil, "", err
	}

	pkUser := &passkeyUser{user: user, credentials: creds}
	options, sessionData, err := s.web.BeginRegistration(pkUser)
	if err != nil {
		return nil, "", err
	}
	sessID, err := s.store.Save(sessionData)
	if err != nil {
		return nil, "", err
	}
	return &options.Response, sessID, nil
}

// FinishRegistration 校验并落库一条新凭据
func (s *PasskeyService) FinishRegistration(
	ctx context.Context,
	userID uuid.UUID,
	sessionID string,
	rawResponse []byte,
	name string,
) error {
	sessionData, ok := s.store.Load(sessionID)
	if !ok {
		return ErrInvalidCredentials
	}

	parsed, err := protocol.ParseCredentialCreationResponseBytes(rawResponse)
	if err != nil {
		return err
	}

	user, err := s.auth.repo.GetUser(ctx, userID)
	if err != nil {
		return err
	}
	creds, err := s.auth.repo.ListUserCredentials(ctx, userID)
	if err != nil {
		return err
	}
	pkUser := &passkeyUser{user: user, credentials: creds}

	cred, err := s.web.CreateCredential(pkUser, *sessionData, parsed)
	if err != nil {
		return err
	}
	return s.auth.repo.SavePasskey(ctx, userID, cred, name)
}

// =============================================================================
// 登录（无密码登录）
// =============================================================================

// BeginLogin 服务端按邮箱定位用户并生成 challenge；客户端拿到后用 navigator.credentials.get({publicKey:...})
// 验证；finish 时把 assertion response 回传服务端。
func (s *PasskeyService) BeginLogin(
	ctx context.Context,
	email string,
) (*protocol.PublicKeyCredentialRequestOptions, string, error) {
	user, err := s.findUserForLogin(ctx, email)
	if err != nil {
		return nil, "", err
	}
	creds, err := s.auth.repo.ListUserCredentials(ctx, user.ID)
	if err != nil {
		return nil, "", err
	}
	if len(creds) == 0 {
		return nil, "", ErrInvalidCredentials
	}
	pkUser := &passkeyUser{user: user, credentials: creds}
	options, sessionData, err := s.web.BeginLogin(pkUser)
	if err != nil {
		return nil, "", err
	}
	sessID, err := s.store.Save(sessionData)
	if err != nil {
		return nil, "", err
	}
	return &options.Response, sessID, nil
}

// FinishLogin 校验 assertion 通过后签发会话
func (s *PasskeyService) FinishLogin(
	ctx context.Context,
	sessionID string,
	email string,
	rawResponse []byte,
	deviceName, platform string,
) (*LoginResult, error) {
	sessionData, ok := s.store.Load(sessionID)
	if !ok {
		return nil, ErrInvalidCredentials
	}

	user, err := s.findUserForLogin(ctx, email)
	if err != nil {
		return nil, err
	}
	creds, err := s.auth.repo.ListUserCredentials(ctx, user.ID)
	if err != nil {
		return nil, err
	}
	pkUser := &passkeyUser{user: user, credentials: creds}

	parsed, err := protocol.ParseCredentialRequestResponseBytes(rawResponse)
	if err != nil {
		return nil, err
	}

	cred, err := s.web.ValidateLogin(pkUser, *sessionData, parsed)
	if err != nil {
		return nil, err
	}
	if err := s.auth.repo.UpdatePasskeySignCount(ctx, cred.ID, cred.Authenticator.SignCount); err != nil {
		return nil, err
	}
	return s.auth.IssueSessionForUser(ctx, user, deviceName, platform)
}

// findUserForLogin 由 email 查；未来 discoverable credentials 走 user.handle 时再扩展
func (s *PasskeyService) findUserForLogin(ctx context.Context, email string) (*User, error) {
	if email == "" {
		return nil, ErrInvalidCredentials
	}
	user, err := s.auth.repo.UpsertUserByEmail(ctx, email)
	if err != nil {
		return nil, err
	}
	if user == nil {
		return nil, errors.New("user not found")
	}
	return user, nil
}

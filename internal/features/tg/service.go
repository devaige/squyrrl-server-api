package tg

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/squyrrl/api/internal/features/snippet"
)

// tokenTTL 比手输码时代的 10 分钟更短：deep link 是「点开就用」的，
// 用户不需要在两个应用之间搬运字符串，5 分钟足够走完唤起 TG 的全程，
// 而更短的窗口意味着截图外流的链接更快失效。
const tokenTTL = 5 * time.Minute

type Service struct {
	repo        *Repo
	snipSvc     *snippet.Service
	botUsername string
}

func NewService(repo *Repo, snipSvc *snippet.Service, botUsername string) *Service {
	return &Service{repo: repo, snipSvc: snipSvc, botUsername: botUsername}
}

// =============================================================================
// 绑定：App 签发令牌 → 用户点 deep link → Bot 核销
// =============================================================================

// IssueBindingLink 用户端调用：为已登录用户签发一枚令牌并拼成 deep link。
// 链接由服务端拼装，客户端只负责渲染二维码 —— 换 Bot 只改一处环境变量。
func (s *Service) IssueBindingLink(ctx context.Context, userID uuid.UUID) (*BindingLink, error) {
	if s.botUsername == "" {
		return nil, ErrBotUnconfigured
	}
	token, exp, err := s.repo.IssueToken(ctx, userID, tokenTTL)
	if err != nil {
		return nil, err
	}
	return &BindingLink{
		Token:     token,
		URL:       fmt.Sprintf("https://t.me/%s?start=%s", s.botUsername, token),
		ExpiresAt: exp,
	}, nil
}

// Bind Bot 端调用：核销令牌，把提交上来的 TG 号绑到令牌所属的 Squyrrl 账户。
func (s *Service) Bind(ctx context.Context, token string, id TGIdentity) (*Binding, error) {
	userID, err := s.repo.ConsumeToken(ctx, token)
	if err != nil {
		return nil, err
	}

	deviceID, err := s.repo.CreateTelegramDevice(ctx, userID, deviceName(id))
	if err != nil {
		return nil, err
	}
	if err := s.repo.CreateBinding(ctx, id, userID, deviceID); err != nil {
		// 绑定失败（令牌有效但该 TG 号已绑到别的账户）时回收刚建的设备占位，
		// 否则用户的设备列表里会留下一台永远不会被使用的「Telegram」。
		s.repo.RevokeDevice(ctx, deviceID)
		return nil, err
	}
	return s.repo.FindByTGUser(ctx, id.TGUserID)
}

func (s *Service) ListBindings(ctx context.Context, userID uuid.UUID) ([]Binding, error) {
	return s.repo.ListByUser(ctx, userID)
}

// Unbind 用户端解绑
func (s *Service) Unbind(ctx context.Context, userID, bindingID uuid.UUID) error {
	deviceID, err := s.repo.DeleteByID(ctx, userID, bindingID)
	if err != nil {
		return err
	}
	s.repo.RevokeDevice(ctx, deviceID)
	return nil
}

// UnbindByTGUser Bot 端解绑（/unbind 命令）
func (s *Service) UnbindByTGUser(ctx context.Context, tgUserID int64) error {
	deviceID, err := s.repo.DeleteByTGUser(ctx, tgUserID)
	if err != nil {
		return err
	}
	s.repo.RevokeDevice(ctx, deviceID)
	return nil
}

// Status Bot 端查询绑定状态：让 Bot 在把消息正文发出来之前先问一句，
// 未绑定用户的内容就不必进入 API 的请求体和日志。
func (s *Service) Status(ctx context.Context, tgUserID int64) (*BindingStatusResponse, error) {
	b, err := s.repo.FindByTGUser(ctx, tgUserID)
	if errors.Is(err, ErrNotBound) {
		return &BindingStatusResponse{Bound: false}, nil
	}
	if err != nil {
		return nil, err
	}
	return &BindingStatusResponse{Bound: true, TGUsername: deref(b.TGUsername)}, nil
}

func deviceName(id TGIdentity) string {
	switch {
	case id.Username != "":
		return "Telegram @" + id.Username
	case id.Name != "":
		return "Telegram " + id.Name
	default:
		return fmt.Sprintf("Telegram %d", id.TGUserID)
	}
}

// =============================================================================
// 转发碎片
// =============================================================================

// CreateForwardedSnippet Bot 端调用：按 tg_user_id 找绑定，代为创建碎片
func (s *Service) CreateForwardedSnippet(
	ctx context.Context, tgUserID int64, in *ForwardedSnippetInput,
) (*snippet.Snippet, error) {
	binding, err := s.repo.FindByTGUser(ctx, tgUserID)
	if err != nil {
		return nil, err
	}

	source := map[string]any{
		"provider":   "telegram",
		"device_id":  binding.DeviceID,
		"tg_user_id": tgUserID,
	}
	if in.ChatID != 0 {
		source["tg_chat_id"] = in.ChatID
	}
	if in.MessageID != 0 {
		source["tg_message_id"] = in.MessageID
	}
	sourceData, _ := json.Marshal(source)

	snippetType := in.Type
	if snippetType == "" {
		snippetType = "normal"
	}
	textFormat := in.TextFormat
	if textFormat == "" {
		textFormat = "plain"
	}

	elements := make([]snippet.ElementInput, 0, len(in.FileIDs))
	for i, fid := range in.FileIDs {
		elements = append(elements, snippet.ElementInput{FileID: fid, OrderIdx: i})
	}

	create := &snippet.CreateInput{
		Type:        snippetType,
		Subtype:     nilIfEmpty(in.Subtype),
		Title:       nilIfEmpty(in.Title),
		Description: nilIfEmpty(in.Description),
		TextContent: in.TextContent,
		TextFormat:  textFormat,
		Payload:     in.Payload,
		SourceType:  "device",
		SourceData:  sourceData,
		Elements:    elements,
	}

	res, err := s.snipSvc.Create(ctx, binding.UserID, create)
	if err != nil {
		return nil, err
	}
	s.repo.TouchLastUsed(ctx, tgUserID)
	return res, nil
}

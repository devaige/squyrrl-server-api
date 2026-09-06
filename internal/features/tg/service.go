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

const codeTTL = 10 * time.Minute

type Service struct {
	repo    *Repo
	snipSvc *snippet.Service
}

func NewService(repo *Repo, snipSvc *snippet.Service) *Service {
	return &Service{repo: repo, snipSvc: snipSvc}
}

// =============================================================================
// 绑定：Bot 签发码 → 用户在 App 内兑换
// =============================================================================

// IssueCode Bot 端调用：为一个尚未绑定的 TG 用户签发绑定码
func (s *Service) IssueCode(ctx context.Context, id TGIdentity) (*BindingCode, error) {
	if _, err := s.repo.FindByTGUser(ctx, id.TGUserID); err == nil {
		return nil, ErrAlreadyBound
	} else if !errors.Is(err, ErrNotBound) {
		return nil, err
	}
	return s.repo.IssueCode(ctx, id, codeTTL)
}

// Redeem 用户端调用：已登录用户输入码，把码代表的 TG 号绑到自己账户
func (s *Service) Redeem(ctx context.Context, userID uuid.UUID, code string) (*Binding, error) {
	id, err := s.repo.ConsumeCode(ctx, code)
	if err != nil {
		return nil, err
	}

	deviceID, err := s.repo.CreateTelegramDevice(ctx, userID, deviceName(id))
	if err != nil {
		return nil, err
	}
	if err := s.repo.CreateBinding(ctx, id, userID, deviceID); err != nil {
		// 绑定失败（码有效但该 TG 号已被别人抢先绑走）时回收刚建的设备占位，
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

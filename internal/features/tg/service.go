package tg

import (
	"context"
	"encoding/json"
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

// IssueCode 用户端调用：为已登录用户生成 10 分钟有效的绑定码
func (s *Service) IssueCode(ctx context.Context, userID uuid.UUID) (*BindingCode, error) {
	deviceID, err := s.repo.CreateOrReuseTelegramDevice(ctx, userID)
	if err != nil {
		return nil, err
	}
	return s.repo.IssueCode(ctx, userID, deviceID, codeTTL)
}

// CompleteBinding Bot 端调用：用绑定码 + tg_user_id 完成绑定
func (s *Service) CompleteBinding(ctx context.Context, code string, tgUserID int64) (uuid.UUID, error) {
	userID, deviceID, err := s.repo.ConsumeCode(ctx, code)
	if err != nil {
		return uuid.Nil, err
	}
	if err := s.repo.CreateBinding(ctx, tgUserID, userID, deviceID); err != nil {
		return uuid.Nil, err
	}
	return userID, nil
}

// CreateForwardedSnippet Bot 端调用：根据 tg_user_id 找绑定，代为创建 snippet
func (s *Service) CreateForwardedSnippet(ctx context.Context, tgUserID int64, in *ForwardedSnippetInput) (*snippet.Snippet, error) {
	binding, err := s.repo.FindByTGUser(ctx, tgUserID)
	if err != nil {
		return nil, err
	}

	sourceData, _ := json.Marshal(map[string]any{
		"provider":   "telegram",
		"device_id":  binding.DeviceID,
		"tg_user_id": tgUserID,
	})

	snippetType := in.Type
	if snippetType == "" {
		snippetType = "normal"
	}
	textFormat := in.TextFormat
	if textFormat == "" {
		textFormat = "plain"
	}

	create := &snippet.CreateInput{
		Type:        snippetType,
		Title:       nilIfEmpty(in.Title),
		Description: nilIfEmpty(in.Description),
		TextContent: in.TextContent,
		TextFormat:  textFormat,
		SourceType:  "device",
		SourceData:  sourceData,
	}

	res, err := s.snipSvc.Create(ctx, binding.UserID, create)
	if err != nil {
		return nil, err
	}
	s.repo.TouchLastUsed(ctx, tgUserID)
	return res, nil
}

func nilIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

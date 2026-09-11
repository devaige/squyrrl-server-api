package tg

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/google/uuid"

	"github.com/squyrrl/api/internal/features/snippet"
)

// tokenTTL 比手输码时代的 10 分钟更短：deep link 是「点开就用」的，
// 用户不需要在两个应用之间搬运字符串，5 分钟足够走完唤起 TG 的全程，
// 而更短的窗口意味着截图外流的链接更快失效。
const tokenTTL = 5 * time.Minute

// QuotaChecker 是 quota.Service 的窄接口。用接口而非直接依赖，是为了不让
// tg 包在构造期反向依赖 quota —— 后者已经依赖 wallet 读档位，绕成环。
type QuotaChecker interface {
	CheckBindingCreate(ctx context.Context, userID uuid.UUID, platform string) error
	CheckUpload(ctx context.Context, userID uuid.UUID, size int64) error
}

type Service struct {
	repo        *Repo
	snipSvc     *snippet.Service
	quota       QuotaChecker
	botUsername string
}

func NewService(repo *Repo, snipSvc *snippet.Service, quota QuotaChecker, botUsername string) *Service {
	return &Service{repo: repo, snipSvc: snipSvc, quota: quota, botUsername: botUsername}
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
	token, exp, err := s.repo.IssueToken(ctx, userID, PlatformTelegram, tokenTTL)
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
	userID, err := s.repo.ConsumeToken(ctx, token, PlatformTelegram)
	if err != nil {
		return nil, err
	}

	acc := id.identity()

	// 档位门槛卡在核销之后、建设备之前。放在核销之前做不到 —— 那时还不知道这枚
	// 令牌属于谁；放在建绑定之后则要多回滚一次。令牌被白白消费掉是可接受的代价：
	// 用户升档后重点一次按钮即可，而这条路径本来就不该走通。
	if s.quota != nil {
		if err := s.quota.CheckBindingCreate(ctx, userID, acc.Platform); err != nil {
			return nil, err
		}
	}

	deviceID, err := s.repo.CreateBindingDevice(ctx, userID, PlatformTelegram, deviceName(id))
	if err != nil {
		return nil, err
	}
	if err := s.repo.CreateBinding(ctx, acc, userID, deviceID); err != nil {
		// 绑定失败（令牌有效但该 TG 号已绑到别的账户）时回收刚建的设备占位，
		// 否则用户的设备列表里会留下一台永远不会被使用的「Telegram」。
		s.repo.RevokeDevice(ctx, deviceID)
		return nil, err
	}
	return s.repo.FindByAccount(ctx, acc.Platform, acc.UserID)
}

// CheckUploadFor 在 Bot 代传文件之前，按 tg_user_id 找到账户并校验存储配额。
//
// 这条路径此前完全没有配额约束，而且成因是结构性的：`/internal/tg/files` 只带
// 哈希与大小，**压根不知道是谁的文件** —— 账户要到后面建碎片那一步才由
// tg_user_id 解析出来。于是字节先落 R2，配额不足在建碎片时才暴露，
// 留下一批要等 GC 的孤儿对象，而它们已经开始计费了。
//
// 修法是把「谁」提前到上传这一步：Bot 本来就知道 tg_user_id，带上即可。
func (s *Service) CheckUploadFor(ctx context.Context, tgUserID int64, size int64) error {
	if s.quota == nil {
		return nil
	}
	b, err := s.repo.FindByAccount(ctx, PlatformTelegram, strconv.FormatInt(tgUserID, 10))
	if err != nil {
		return err
	}
	return s.quota.CheckUpload(ctx, b.UserID, size)
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
	deviceID, err := s.repo.DeleteByAccount(ctx, PlatformTelegram, strconv.FormatInt(tgUserID, 10))
	if err != nil {
		return err
	}
	s.repo.RevokeDevice(ctx, deviceID)
	return nil
}

// Status Bot 端查询绑定状态：让 Bot 在把消息正文发出来之前先问一句，
// 未绑定用户的内容就不必进入 API 的请求体和日志。
func (s *Service) Status(ctx context.Context, tgUserID int64) (*BindingStatusResponse, error) {
	b, err := s.repo.FindByAccount(ctx, PlatformTelegram, strconv.FormatInt(tgUserID, 10))
	if errors.Is(err, ErrNotBound) {
		return &BindingStatusResponse{Bound: false}, nil
	}
	if err != nil {
		return nil, err
	}
	return &BindingStatusResponse{Bound: true, TGUsername: deref(b.Username)}, nil
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
	binding, err := s.repo.FindByAccount(ctx, PlatformTelegram, strconv.FormatInt(tgUserID, 10))
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
	s.repo.TouchLastUsed(ctx, PlatformTelegram, strconv.FormatInt(tgUserID, 10))
	return res, nil
}

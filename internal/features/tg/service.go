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
	"github.com/squyrrl/api/internal/infra/ratelimit"
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

// 短码认领的限流。按平台账号（TG 号）分桶而非按 IP：请求全部来自 Bot 那一台
// 机器，IP 没有区分度。
//
// 8 位码有 31^8 ≈ 2^39.6 种，配合 5 分钟 TTL，这个额度下猜中任何一枚活跃码
// 需要的账号数远超注册 TG 号的成本。额度给到 10 是留给真实用户的手误：
// 敲错一两次、把码连着发两遍都不该被挡。
const (
	claimAttemptLimit  = 10
	claimAttemptWindow = 10 * time.Minute
)

type Service struct {
	repo        *Repo
	snipSvc     *snippet.Service
	quota       QuotaChecker
	botUsername string
	claimLimit  *ratelimit.Limiter
}

func NewService(repo *Repo, snipSvc *snippet.Service, quota QuotaChecker, botUsername string) *Service {
	return &Service{
		repo:        repo,
		snipSvc:     snipSvc,
		quota:       quota,
		botUsername: botUsername,
		claimLimit:  ratelimit.New(claimAttemptLimit, claimAttemptWindow),
	}
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
	t, err := s.repo.IssueToken(ctx, userID, PlatformTelegram, tokenTTL)
	if err != nil {
		return nil, err
	}
	return &BindingLink{
		Token:     t.Token,
		URL:       fmt.Sprintf("https://t.me/%s?start=%s", s.botUsername, t.Token),
		ShortCode: t.ShortCode,
		ExpiresAt: t.ExpiresAt,
	}, nil
}

// Bind Bot 端调用：核销令牌，把提交上来的 TG 号绑到令牌所属的 Squyrrl 账户。
// 这是 deep link / 扫码那条路径 —— 载体没经人手搬运，核销即绑定，不要确认。
func (s *Service) Bind(ctx context.Context, token string, id TGIdentity) (*Binding, error) {
	userID, err := s.repo.ConsumeToken(ctx, token, PlatformTelegram)
	if err != nil {
		return nil, err
	}
	acc := id.identity()
	return s.bindIdentity(ctx, userID, acc, deviceNameFor(acc))
}

// bindIdentity 是「令牌已核销，该真正建立绑定了」这一段，由 deep link 与短码确认
// 两条路径共用。抽出来是因为配额门槛、建设备、失败回收这三步的顺序有讲究，
// 两份拷贝迟早会在其中一处分叉。
func (s *Service) bindIdentity(
	ctx context.Context, userID uuid.UUID, acc Identity, devName string,
) (*Binding, error) {
	// 档位门槛卡在核销之后、建设备之前。放在核销之前做不到 —— 那时还不知道这枚
	// 令牌属于谁；放在建绑定之后则要多回滚一次。令牌被白白消费掉是可接受的代价：
	// 用户升档后重点一次按钮即可，而这条路径本来就不该走通。
	if s.quota != nil {
		if err := s.quota.CheckBindingCreate(ctx, userID, acc.Platform); err != nil {
			return nil, err
		}
	}

	deviceID, err := s.repo.CreateBindingDevice(ctx, userID, acc.Platform, devName)
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

// =============================================================================
// 短码路径：Bot 认领 → App 确认
// =============================================================================

// ClaimBindingCode Bot 端调用：登记「这个 TG 号拿着这枚短码来申请绑定」。
//
// **这里不建立任何绑定**，也因此不查配额门槛 —— 认领不是一次授权，它只是把
// 申请者的身份摆到账户主人面前。真正的门槛在 ConfirmBindingClaim。
func (s *Service) ClaimBindingCode(ctx context.Context, code string, id TGIdentity) (time.Time, error) {
	acc := id.identity()
	if !s.claimLimit.Allow(acc.Platform + ":" + acc.UserID) {
		return time.Time{}, ErrTooManyAttempts
	}
	return s.repo.ClaimToken(ctx, code, PlatformTelegram, acc)
}

// PendingBindingClaim 用户端轮询：当前有没有人拿着我的短码在等确认。
func (s *Service) PendingBindingClaim(ctx context.Context, userID uuid.UUID) (*PendingClaim, error) {
	return s.repo.FindPendingClaim(ctx, userID, PlatformTelegram)
}

// ConfirmBindingClaim 用户端确认：核销令牌并建立绑定。
// expectPlatformUserID 是 App 展示给用户看的那个账号，必须与待确认的一致。
func (s *Service) ConfirmBindingClaim(
	ctx context.Context, userID uuid.UUID, expectPlatformUserID string,
) (*Binding, error) {
	acc, err := s.repo.ConsumeClaimedToken(ctx, userID, PlatformTelegram, expectPlatformUserID)
	if err != nil {
		return nil, err
	}
	return s.bindIdentity(ctx, userID, acc, deviceNameFor(acc))
}

// RejectBindingClaim 用户端拒绝
func (s *Service) RejectBindingClaim(ctx context.Context, userID uuid.UUID) error {
	return s.repo.RejectClaim(ctx, userID, PlatformTelegram)
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

// deviceNameFor 给绑定占位设备起名。平台前缀由 Identity.Platform 决定而不是写死
// "Telegram"：这个名字直接出现在用户的设备列表里，接入第二个平台时最不该还叫
// Telegram，而那种错误不会有任何测试发现。
func deviceNameFor(acc Identity) string {
	label := acc.Platform
	if acc.Platform == PlatformTelegram {
		label = "Telegram"
	}
	switch {
	case acc.Username != "":
		return label + " @" + acc.Username
	case acc.Name != "":
		return label + " " + acc.Name
	default:
		return label + " " + acc.UserID
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

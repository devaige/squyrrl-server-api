package file

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"net/url"
	"path"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/squyrrl/api/internal/features/file/thumb"
	"github.com/squyrrl/api/internal/infra/storage"
)

// UploadQuota 是 quota.Service 的窄接口：上传前判定这个文件放不放得下。
//
// 用接口而非直接依赖 quota.Service，理由和其它几处一样 —— 让 file 的单测
// 不必拖进一个数据库连接池，也让「没配额检查器」在构造期就是一个显式的选择
// 而不是一次忘记注入。
type UploadQuota interface {
	CheckUpload(ctx context.Context, userID uuid.UUID, size int64) error
}

type Service struct {
	repo    *Repo
	storage *storage.Client
	quota   UploadQuota

	// 边缘配置（ADR-069 上传 / ADR-070 下载），两项都必填。任一为空 ⇒ 上传与下载
	// 一起不可用（fail-closed）：签发端点返回 503 —— 而**不是**退回让字节穿过 api。
	// 那种回退档位一旦存在，漏配就等于持续付出网带宽费用，且毫无报错。
	edgeBase    string
	tokenSecret string
}

func NewService(repo *Repo, st *storage.Client, q UploadQuota, edgeBase, tokenSecret string) *Service {
	return &Service{repo: repo, storage: st, quota: q, edgeBase: edgeBase, tokenSecret: tokenSecret}
}

// EdgeEnabled 报告边缘是否可用：边缘地址与令牌密钥缺一不可。上传与下载共用这一个
// 开关，因为它们共用同一个 Worker、同一个密钥、同一个自定义域 —— 拆成两个开关就多出
// 「能传不能取」这类没人想要的中间态。
//
// dev 也要显式配 edgeBase（指向 api 自己的 /edge），好让「有没有边缘」是一个二值问题，
// 而不是「有边缘 / 有个悄悄改吃服务器带宽的替身」。
func (s *Service) EdgeEnabled() bool {
	return s.edgeBase != "" && s.tokenSecret != ""
}

// VerifyUploadToken 供内嵌边缘校验令牌；生产由 Worker 用同一套算法自行校验。
func (s *Service) VerifyUploadToken(token string) (*UploadClaims, error) {
	if s.tokenSecret == "" {
		return nil, ErrEdgeDisabled
	}
	return VerifyUploadToken(s.tokenSecret, token)
}

// VerifyDownloadToken 同上，用途为下载。两种令牌签名互不通用（见 token.go 的用途常量）。
func (s *Service) VerifyDownloadToken(token string) (*DownloadClaims, error) {
	if s.tokenSecret == "" {
		return nil, ErrEdgeDisabled
	}
	return VerifyDownloadToken(s.tokenSecret, token)
}

// 对象 key 的前缀。**用前缀区分类型，而不是分桶**（2026-09-08 用户决策）：
// key 是内容寻址的（hex(cipher_hash)，SHA-256），本就不存在命名冲突，分桶换不来隔离；
// 而多桶会逼 files 行多存一列「在哪个桶」，还要把桶选择器塞进上传令牌 —— 那个令牌刚做过
// 用途域分离（ADR-070），不该再长出可被外部影响的字段。R2 的生命周期规则支持前缀过滤，
// 「不同类型不同保留策略」这个常见的分桶理由在这里也不成立。
// 真正值得单开一个桶的只有备份 / 导出这类 **file GC 绝不该看见** 的对象。
const (
	blobPrefix  = "blob/"
	thumbPrefix = "thumb/"
)

// StorageKeyFor 由 cipher_hash 推导文件本体的对象 key。
// 全局唯一由哈希保证，前缀只承担分类。
func StorageKeyFor(cipherHash []byte) string {
	return blobPrefix + hex.EncodeToString(cipherHash)
}

// ThumbnailKeyFor 由文件本体的 key 推导缩略图 key。
//
// 取 path.Base 而不是直接拼接：storage_key 带 blob/ 前缀，直接拼会得到
// thumb/blob/<hex>.jpg —— 两类对象的前缀嵌套起来，按前缀配生命周期规则时会互相打架。
// 顺带兼容不带前缀的历史 key。
func ThumbnailKeyFor(storageKey string) string {
	return thumbPrefix + path.Base(storageKey) + ".jpg"
}

// Check 仅查重，不上传
func (s *Service) Check(ctx context.Context, plainHash []byte) (*File, bool, error) {
	f, err := s.repo.FindByPlainHash(ctx, plainHash)
	if errors.Is(err, ErrNotFound) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return f, true, nil
}

// Upload 接收混淆字节并落库；若已存在直接返回，不重复上传
//   - body 限制为 declaredSize 字节（避免吃光内存）
//   - 校验上传字节的 SHA-256 与 declaredCipherHash 一致
func (s *Service) Upload(
	ctx context.Context,
	plainHash, cipherHash []byte,
	declaredSize int64,
	mime string,
	body io.Reader,
) (*File, error) {
	if existing, ok, err := s.Check(ctx, plainHash); err != nil {
		return nil, err
	} else if ok {
		// 全局已有该明文哈希对应的对象，直接复用
		return existing, nil
	}

	limited := io.LimitReader(body, declaredSize+1) // +1 用于检测超长
	data, err := io.ReadAll(limited)
	if err != nil {
		return nil, err
	}
	if int64(len(data)) != declaredSize {
		return nil, ErrSizeMismatch
	}

	actual := sha256.Sum256(data)
	if !bytes.Equal(actual[:], cipherHash) {
		return nil, ErrCipherMismatch
	}

	storageKey := StorageKeyFor(cipherHash)
	if err := s.storage.Put(ctx, storageKey, bytes.NewReader(data), declaredSize, mime); err != nil {
		return nil, err
	}

	f, err := s.repo.Insert(ctx, plainHash, cipherHash, declaredSize, mime, s.storage.Bucket(), storageKey)
	if err != nil {
		return nil, err
	}

	// 仅对图片 mime 触发缩略图生成。客户端混淆字节解码会失败，归入「不支持」分支，
	// 不会写 thumbnail_key，客户端走原始 download fallback。
	if strings.HasPrefix(mime, "image/") {
		go s.generateThumbnailAsync(f.ID, storageKey, data)
	}

	return f, nil
}

// generateThumbnailAsync 在 Upload 成功后异步生成图片缩略图。
//   - 解码失败（混淆字节 / 非主流格式）→ 跳过；thumbnail_key 保持 NULL
//   - 用独立 context.Background()，请求上下文取消不影响后台任务
//   - 上限 30s（draw.CatmullRom 在 8k 像素图上单核 ~1s 内）
func (s *Service) generateThumbnailAsync(fileID [16]byte, storageKey string, plainBytes []byte) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	thumbBytes, err := thumb.Generate(bytes.NewReader(plainBytes))
	if err != nil {
		// 包括 ErrUnsupported；mime 是 image/* 但字节是 cipher 时会落到这里
		slog.Debug("thumb 跳过", "file_id", fileID, "err", err)
		return
	}

	key := ThumbnailKeyFor(storageKey)
	if err := s.storage.Put(ctx, key, bytes.NewReader(thumbBytes),
		int64(len(thumbBytes)), "image/jpeg"); err != nil {
		slog.Warn("thumb 上传对象存储失败", "file_id", fileID, "err", err)
		return
	}

	if err := s.repo.SetThumbnailKey(ctx, fileID, key); err != nil {
		slog.Warn("thumb 写库失败", "file_id", fileID, "err", err)
		return
	}
}

// GetMetadata 只返 file 元数据（JSON），不读字节
func (s *Service) GetMetadata(ctx context.Context, id [16]byte) (*File, error) {
	return s.repo.Get(ctx, id)
}

// IssueTicket 签发一组短期边缘直读 URL（ADR-070）。
//
// api 在这里只做「查库 + 签名」两件事，**不碰字节**。旧的 /download、/thumb 是
// io.Copy(c.Writer, storage.Get(...))：功能上没问题，但每一次预览、每一次打开附件
// 都要把整个对象从 R2 拉进 api 再推给客户端，同一份流量付两次钱（R2 出网 + 服务器出网），
// 而读的次数天然远多于写。改成签票据后，字节只走 R2 → Worker → 客户端，
// 这段路径在 Cloudflare 内部且 Worker 出网不计费。
func (s *Service) IssueTicket(ctx context.Context, id [16]byte) (*TicketResponse, error) {
	if !s.EdgeEnabled() {
		return nil, ErrEdgeDisabled
	}
	f, err := s.repo.Get(ctx, id)
	if err != nil {
		return nil, err
	}

	// 两个 URL 共用同一个到期时刻，客户端只需要记一个数就能判断整张票是否还新鲜
	expiresAt := time.Now().Add(downloadTTL)
	blob, err := s.signBlobURL(f.StorageKey, f.Mime, expiresAt)
	if err != nil {
		return nil, err
	}
	resp := &TicketResponse{File: f, BlobURL: blob, ExpiresAt: expiresAt.Unix()}

	if f.ThumbnailKey != "" {
		// 缩略图是服务端生成的真 JPEG（没混淆），mime 恒定，不跟随原文件
		thumb, err := s.signBlobURL(f.ThumbnailKey, "image/jpeg", expiresAt)
		if err != nil {
			return nil, err
		}
		resp.ThumbURL = thumb
	}
	return resp, nil
}

// signBlobURL 把令牌放在查询串而不是要求调用方设 Authorization 头：
// 这样 URL 自包含，能直接喂给 Image.network / <img src> / 浏览器下载，
// 不必每个消费点都改成「先构造带头的请求」。代价是令牌会进访问日志，
// 由 downloadTTL 的短窗口兜住 —— 边缘两种取法都认（见 edge_handler.go）。
func (s *Service) signBlobURL(key, mime string, expiresAt time.Time) (string, error) {
	tok, err := SignDownloadToken(s.tokenSecret, DownloadClaims{
		Key: key, Mime: mime, ExpiresAt: expiresAt.Unix(),
	})
	if err != nil {
		return "", err
	}
	return s.edgeBase + "/v1/blob?t=" + url.QueryEscape(tok), nil
}

// =============================================================================
// 直传（ADR-069）：api 只签发与收尾，字节经边缘 Worker 写入 R2
// =============================================================================

// IssueIntent 是直传的第一步：去重命中就直接返回既有文件，未命中才开意图并签令牌。
//
// 刻意**不**检查「同一 plain_hash 是否正在上传中」。storage_key = blob/hex(cipher_hash)
// 是内容寻址，两个客户端并发写同一 key 的结果逐字节相同，重复只多花一次带宽；
// 而阻塞式的 in-flight 检查要引入租约、等待方轮询、上传方猝死后的接管，
// 复杂度远超它省下的那点流量。
func (s *Service) IssueIntent(ctx context.Context, in IntentRequest) (*IntentResponse, error) {
	if !s.EdgeEnabled() {
		return nil, ErrEdgeDisabled
	}
	if err := validateThumb(in.Mime, in.SizeBytes, in.ThumbSizeBytes, in.ThumbCipherHash); err != nil {
		return nil, err
	}

	if existing, ok, err := s.Check(ctx, in.PlainHash); err != nil {
		return nil, err
	} else if ok {
		// 去重命中：不传字节，但这个用户的占用照样会涨（ADR-075 的「每个引用者全额计」）。
		// 仍然过一遍配额 —— 否则「别人传过的文件」就是一条绕开额度的免费通道。
		if s.quota != nil {
			if err := s.quota.CheckUpload(ctx, in.UserID, existing.SizeBytes); err != nil {
				return nil, err
			}
		}
		// 命中时**不**签缩略图票，即便这行 files 的 thumbnail_key 还是空的。
		// 那份对象的 key 由**已存在**的 cipher_hash 推导，而调用方手上的缩略图
		// 是按自己刚算出的 cipher_hash 加密的 —— 两者对不上。补历史数据的缩略图
		// 是一条独立的回填链路，不该借一次去重命中顺手做半件。
		return &IntentResponse{Exists: true, File: existing}, nil
	}

	// 配额卡在这里，是因为**这是服务端最后一次能说话的时刻**：签发之后字节直达
	// R2，api 再也看不到它们（ADR-069）。放到 commit 去查就太晚了 ——
	// 那时超出去的字节已经躺在 R2 上，按月计费。
	if s.quota != nil {
		if err := s.quota.CheckUpload(ctx, in.UserID, in.SizeBytes); err != nil {
			return nil, err
		}
	}

	storageKey := StorageKeyFor(in.CipherHash)
	partCount := PartCountFor(in.SizeBytes)

	var r2UploadID string
	if partCount > 1 {
		id, err := s.storage.InitMultipart(ctx, storageKey, in.Mime)
		if err != nil {
			return nil, err
		}
		r2UploadID = id
	}

	expiresAt := time.Now().Add(intentTTL)
	it, err := s.repo.InsertIntent(ctx, &UploadIntent{
		UserID: in.UserID, PlainHash: in.PlainHash, CipherHash: in.CipherHash,
		SizeBytes: in.SizeBytes, Mime: in.Mime, StorageKey: storageKey,
		R2UploadID: r2UploadID, PartSize: PartSize, PartCount: partCount,
		ExpiresAt:       expiresAt,
		ThumbCipherHash: in.ThumbCipherHash, ThumbSizeBytes: in.ThumbSizeBytes,
	})
	if err != nil {
		// 意图落库失败而 multipart 已开：立刻 abort，否则那些分片没人认领也没人计费提醒
		if r2UploadID != "" {
			if aerr := s.storage.AbortMultipart(ctx, storageKey, r2UploadID); aerr != nil {
				slog.Warn("意图落库失败后 abort multipart 也失败（留下分片 dust）",
					"key", storageKey, "err", aerr)
			}
		}
		return nil, err
	}

	token, err := SignUploadToken(s.tokenSecret, UploadClaims{
		IntentID:   it.ID.String(),
		Key:        storageKey,
		UploadID:   r2UploadID,
		SizeBytes:  in.SizeBytes,
		PartSize:   PartSize,
		PartCount:  partCount,
		CipherHash: hex.EncodeToString(in.CipherHash),
		ExpiresAt:  expiresAt.Unix(),
	})
	if err != nil {
		return nil, err
	}

	resp := &IntentResponse{
		Exists:   false,
		IntentID: it.ID.String(),
		// 恒为非空（EdgeEnabled 已挡住空值）。dev 指向 api 自身的 /edge、
		// 生产指向 Worker 自定义域，客户端两种环境共用同一条代码路径，只是终点不同。
		UploadURL: s.edgeBase,
		Token:     token,
		PartSize:  PartSize,
		PartCount: partCount,
	}

	if len(in.ThumbCipherHash) > 0 {
		// 缩略图恒走单片（MaxThumbBytes 远小于 PartSize），所以 UploadID 留空 ——
		// 边缘据此把它路由到 /v1/single 那条分支并做整体哈希校验。
		// 边缘因此一行都不用改：key / sz / ch 本来就是令牌里三个互相独立的字段。
		thumbToken, err := SignUploadToken(s.tokenSecret, UploadClaims{
			IntentID:   it.ID.String(),
			Key:        ThumbnailKeyFor(storageKey),
			SizeBytes:  in.ThumbSizeBytes,
			PartSize:   in.ThumbSizeBytes,
			PartCount:  1,
			CipherHash: hex.EncodeToString(in.ThumbCipherHash),
			ExpiresAt:  expiresAt.Unix(),
		})
		if err != nil {
			return nil, err
		}
		resp.ThumbToken = thumbToken
	}
	return resp, nil
}

// validateThumb 校验一次缩略图声明。thumbHash 为空 = 本次不带缩略图，合法且常见。
//
// 三条规则各挡一件事：
//   - 只有图片能带缩略图 —— 给一份 PDF 签出 thumb_url，客户端会拿它当图去渲染；
//   - 上限 MaxThumbBytes —— 缩略图字节不计存储配额，总得有个天花板；
//   - 必须严格小于本体 —— 这条才是真正把「未计费面积」框死的那一条。
//
// 为什么不把缩略图计进配额：那要给 files 加一列 thumb_size_bytes、让 quota.Storage()
// 多一次 SUM，再让降级生命周期与 file GC 各自重新考虑一遍它 —— 换来的精度是千分之几
// （4 MB 照片配 20 KB 缩略图）。改用「缩略图必须比本体小」这条不变式之后，未计费字节
// 恒不超过已计费字节：最坏情况 R2 实际占用是配额的 2 倍，现实中是 1.005 倍。
// 一条 if 换掉一整条会随时间分叉的核算路径。
func validateThumb(mime string, size, thumbSize int64, thumbHash []byte) error {
	if len(thumbHash) == 0 {
		return nil
	}
	if !strings.HasPrefix(mime, "image/") {
		return ErrThumbNotImage
	}
	if thumbSize <= 0 || thumbSize > MaxThumbBytes || thumbSize >= size {
		return ErrThumbTooLarge
	}
	return nil
}

// Commit 是直传的收尾：合并分片、核对 R2 里的实际字节数、写 files 行。
func (s *Service) Commit(
	ctx context.Context,
	userID uuid.UUID,
	intentID uuid.UUID,
	parts []CommitPart,
) (*File, error) {
	it, err := s.repo.GetIntentForUser(ctx, intentID, userID)
	if err != nil {
		return nil, err
	}
	if it.Status != "pending" {
		return nil, ErrIntentState
	}

	// 收尾期间别人可能已经把同一份字节传完并 commit 了。files.plain_hash 有唯一约束，
	// 此时再 Insert 必然冲突，所以先让路：丢掉自己这半份 multipart，复用对方的成果。
	if existing, ok, err := s.Check(ctx, it.PlainHash); err != nil {
		return nil, err
	} else if ok {
		s.abortIntentObject(ctx, it)
		_ = s.repo.MarkIntentStatus(ctx, it.ID, "aborted")
		return existing, nil
	}

	if it.R2UploadID != "" {
		ordered, err := orderParts(parts, it.PartCount)
		if err != nil {
			return nil, err
		}
		if err := s.storage.CompleteMultipart(ctx, it.StorageKey, it.R2UploadID, ordered); err != nil {
			return nil, err
		}
	}

	// 以 R2 记录的大小为准，而不是客户端签发时声称的大小。直传之后 api 看不到字节，
	// 这是唯一能确认「真的传了这么多」的地方 —— 配额结算必须挂在这个值上。
	actual, err := s.storage.StatObject(ctx, it.StorageKey)
	if err != nil {
		return nil, err
	}
	if actual != it.SizeBytes {
		if derr := s.storage.Delete(ctx, it.StorageKey); derr != nil {
			slog.Warn("大小不符后删除对象失败（留下 dust）", "key", it.StorageKey, "err", derr)
		}
		// 本体作废，缩略图也必须跟着走。它不能靠 abortIntentObject 顺手清掉 ——
		// 那时 multipart 已经 complete，走 abort 分支必然失败并提前 return。
		s.deleteThumbObject(ctx, it)
		_ = s.repo.MarkIntentStatus(ctx, it.ID, "aborted")
		return nil, ErrSizeMismatch
	}

	f, err := s.repo.Insert(ctx, it.PlainHash, it.CipherHash, it.SizeBytes,
		it.Mime, s.storage.Bucket(), it.StorageKey)
	if err != nil {
		return nil, err
	}
	if err := s.repo.MarkIntentStatus(ctx, it.ID, "committed"); err != nil {
		return nil, err
	}

	// 缩略图由**客户端**在混淆之前生成并随本体一起直传（成本审计 #7）。
	// api 依然一个字节都不碰 —— 它只在这里确认「R2 里确实躺着一份声明过的大小」，
	// 然后把 thumbnail_key 填上。服务端自己生成那条路（ADR-050）对直传数据永远
	// 得不到可解码的字节，留着只服务于 TG 摄取（那条路 api 手里有 body）。
	if len(it.ThumbCipherHash) > 0 {
		s.attachThumbnail(ctx, f, it)
	}
	return f, nil
}

// attachThumbnail 收尾缩略图：核对 R2 里的实际字节数，然后写 thumbnail_key。
//
// 全程不返回错误，因为**缩略图不是收尾的前提**：任何一步失败的正确结果都是
// 「这份文件没有缩略图」，而那是下载侧从第一天起就支持的状态（thumb_url 缺省 =
// 客户端回退原图）。让它失败掉整次 commit，等于拿一份已经躺在 R2 里、已经计过费的
// 本体，去赌一个 30 KB 的优化。
func (s *Service) attachThumbnail(ctx context.Context, f *File, it *UploadIntent) {
	key := ThumbnailKeyFor(it.StorageKey)

	// 与本体同一条规矩：以 R2 记录的大小为准，不信客户端签发时声称的值。
	actual, err := s.storage.StatObject(ctx, key)
	if err != nil {
		slog.Debug("缩略图未传或读取失败，按无缩略图处理", "file_id", f.ID, "err", err)
		return
	}
	if actual != it.ThumbSizeBytes {
		// 对不上就删掉。留着它既不会被任何东西引用，也不会被 file GC 看见
		// —— GC 只删 files.thumbnail_key 指到的对象 —— 那就是一份永久计费的垃圾。
		if derr := s.storage.Delete(ctx, key); derr != nil {
			slog.Warn("缩略图大小不符且删除失败（留下 dust）", "key", key, "err", derr)
		}
		return
	}

	if err := s.repo.SetThumbnailKey(ctx, f.ID, key); err != nil {
		slog.Warn("缩略图写库失败", "file_id", f.ID, "err", err)
		return
	}

	// 回填内存里的这一份。客户端拿回包里的 has_thumbnail 决定要不要把手上那份
	// 刚生成的缩略图也落进本地缓存；这里返回 false，它下次预览就会白下一次原图。
	f.ThumbnailKey = key
	f.HasThumbnail = true
}

// abortIntentObject 丢弃意图对应的 R2 半成品：multipart 走 abort，单片走 delete。
// 两者都是尽力而为，失败只记日志 —— 留下的分片是可计费的 dust，但比中断收尾要好。
func (s *Service) abortIntentObject(ctx context.Context, it *UploadIntent) {
	// 缩略图先清，且无论本体走哪条分支 —— 它恒是一次单片直写，与 multipart 无关。
	s.deleteThumbObject(ctx, it)

	if it.R2UploadID != "" {
		if err := s.storage.AbortMultipart(ctx, it.StorageKey, it.R2UploadID); err != nil {
			slog.Warn("abort multipart 失败（留下分片 dust）",
				"intent", it.ID, "key", it.StorageKey, "err", err)
		}
		return
	}
	if err := s.storage.Delete(ctx, it.StorageKey); err != nil {
		slog.Debug("删除单片对象失败（可能根本没传上来）",
			"intent", it.ID, "key", it.StorageKey, "err", err)
	}
}

// deleteThumbObject 删掉意图声明过的缩略图对象。
//
// 单独成函数是因为它有两个触发点，其中一个不能走 abortIntentObject：本体大小不符
// 那条分支发生在 CompleteMultipart 之后，此时再 abort 必然失败。而这个对象一旦
// 没进 files.thumbnail_key，就彻底脱离了 file GC 的视野 —— 漏删不是 dust，是永久账单。
func (s *Service) deleteThumbObject(ctx context.Context, it *UploadIntent) {
	if len(it.ThumbCipherHash) == 0 {
		return
	}
	key := ThumbnailKeyFor(it.StorageKey)
	if err := s.storage.Delete(ctx, key); err != nil {
		slog.Debug("删除缩略图对象失败（可能根本没传上来）",
			"intent", it.ID, "key", key, "err", err)
	}
}

// orderParts 校验并排序客户端交回的分片清单。
// 序号必须恰好覆盖 1..want，缺号或重号都拒绝 —— R2 的 complete 对缺号的报错
// 不足以让客户端知道该重传哪一片。
func orderParts(parts []CommitPart, want int) ([]storage.CompletePart, error) {
	if len(parts) != want {
		return nil, ErrPartsMismatch
	}
	seen := make(map[int]string, want)
	for _, p := range parts {
		if p.PartNumber < 1 || p.PartNumber > want {
			return nil, ErrPartsMismatch
		}
		if _, dup := seen[p.PartNumber]; dup {
			return nil, ErrPartsMismatch
		}
		// R2 binding 回的 etag 不带引号，S3 XML 里的却带。两边都可能经客户端转手，
		// 统一剥掉再交给 minio 拼 CompleteMultipartUpload，避免 ETag 不匹配。
		seen[p.PartNumber] = strings.Trim(p.ETag, `"`)
	}
	out := make([]storage.CompletePart, 0, want)
	for i := 1; i <= want; i++ {
		etag, ok := seen[i]
		if !ok {
			return nil, ErrPartsMismatch
		}
		out = append(out, storage.CompletePart{PartNumber: i, ETag: etag})
	}
	return out, nil
}

// SweepExpiredIntents 回收过期未收尾的直传：abort/delete R2 侧半成品，再清理旧的终态行。
//
// 这是直传相对中转多出来的一类垃圾。file GC 是从 files 表反查 ref_count=0 的，
// 而「客户端传完却没 commit」的对象在 files 表里根本没有行 —— 唯一能找到它们的
// 线索就是意图表。没有这张表就只能靠全量 ListObjects 对账。
func (s *Service) SweepExpiredIntents(ctx context.Context, limit int) (int, error) {
	victims, err := s.repo.TakeExpiredIntents(ctx, limit)
	if err != nil {
		return 0, err
	}
	for _, it := range victims {
		s.abortIntentObject(ctx, it)
	}
	if _, err := s.repo.PurgeSettledIntents(ctx, 24*time.Hour); err != nil {
		slog.Warn("清理已收尾意图行失败", "err", err)
	}
	return len(victims), nil
}

// RunIntentSweeper 阻塞执行过期意图清理循环；调用方放在独立 goroutine。
//
// 没有并进 storage.GC：那个包位于 file 之下（file 依赖 storage），把意图清理塞进去
// 会造成反向依赖。两个循环各跑各的，共用同一个 interval 配置。
func (s *Service) RunIntentSweeper(ctx context.Context, interval time.Duration) {
	if !s.EdgeEnabled() {
		slog.Info("未启用边缘，跳过上传意图清理循环")
		return
	}
	if interval <= 0 {
		interval = 5 * time.Minute
	}
	t := time.NewTicker(interval)
	defer t.Stop()

	slog.Info("上传意图清理启动", "interval", interval, "ttl", intentTTL)

	sweep := func() {
		n, err := s.SweepExpiredIntents(ctx, 100)
		if err != nil {
			slog.Warn("上传意图清理失败", "err", err)
			return
		}
		if n > 0 {
			slog.Info("上传意图清理完成", "aborted", n)
		}
	}

	sweep()
	for {
		select {
		case <-ctx.Done():
			slog.Info("上传意图清理退出")
			return
		case <-t.C:
			sweep()
		}
	}
}

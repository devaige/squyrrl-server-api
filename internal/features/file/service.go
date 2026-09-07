package file

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/squyrrl/api/internal/features/file/thumb"
	"github.com/squyrrl/api/internal/infra/storage"
)

type Service struct {
	repo    *Repo
	storage *storage.Client

	// 直传（ADR-069）配置。
	//   edgeBase 为空 ⇒ 回包里 upload_url 也为空，客户端理解为「用 api 自己的
	//     /edge 内嵌边缘」——dev 接 MinIO、没有 Cloudflare Worker 时走这条。
	//   tokenSecret 为空 ⇒ 无法签令牌，直传整体不可用（fail-closed）。
	edgeBase    string
	tokenSecret string
}

func NewService(repo *Repo, st *storage.Client, edgeBase, tokenSecret string) *Service {
	return &Service{repo: repo, storage: st, edgeBase: edgeBase, tokenSecret: tokenSecret}
}

// DirectUploadEnabled 报告直传是否可用。只取决于令牌密钥：
// edgeBase 缺省时退到内嵌边缘，而不是退到「没有直传」。
func (s *Service) DirectUploadEnabled() bool { return s.tokenSecret != "" }

// VerifyUploadToken 供内嵌边缘校验令牌；生产由 Worker 用同一套算法自行校验。
func (s *Service) VerifyUploadToken(token string) (*UploadClaims, error) {
	if s.tokenSecret == "" {
		return nil, ErrEdgeDisabled
	}
	return VerifyUploadToken(s.tokenSecret, token)
}

// 缩略图对象 key 前缀
const thumbPrefix = "thumb/"

// ThumbnailKeyFor 生成缩略图在对象存储中的 key
func ThumbnailKeyFor(storageKey string) string {
	return thumbPrefix + storageKey + ".jpg"
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

	storageKey := hex.EncodeToString(cipherHash)
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

// Download 取流；caller 负责 Close
func (s *Service) Download(ctx context.Context, id [16]byte) (io.ReadCloser, *File, error) {
	f, err := s.repo.Get(ctx, id)
	if err != nil {
		return nil, nil, err
	}
	rc, err := s.storage.Get(ctx, f.StorageKey)
	if err != nil {
		return nil, nil, err
	}
	return rc, f, nil
}

// DownloadThumbnail 取缩略图字节流；没有缩略图返回 ErrNoThumbnail
func (s *Service) DownloadThumbnail(ctx context.Context, id [16]byte) (io.ReadCloser, *File, error) {
	f, err := s.repo.Get(ctx, id)
	if err != nil {
		return nil, nil, err
	}
	if f.ThumbnailKey == "" {
		return nil, f, ErrNoThumbnail
	}
	rc, err := s.storage.Get(ctx, f.ThumbnailKey)
	if err != nil {
		return nil, nil, err
	}
	return rc, f, nil
}

// =============================================================================
// 直传（ADR-069）：api 只签发与收尾，字节经边缘 Worker 写入 R2
// =============================================================================

// IssueIntent 是直传的第一步：去重命中就直接返回既有文件，未命中才开意图并签令牌。
//
// 刻意**不**检查「同一 plain_hash 是否正在上传中」。storage_key = hex(cipher_hash)
// 是内容寻址，两个客户端并发写同一 key 的结果逐字节相同，重复只多花一次带宽；
// 而阻塞式的 in-flight 检查要引入租约、等待方轮询、上传方猝死后的接管，
// 复杂度远超它省下的那点流量。
func (s *Service) IssueIntent(
	ctx context.Context,
	userID uuid.UUID,
	plainHash, cipherHash []byte,
	size int64,
	mime string,
) (*IntentResponse, error) {
	if !s.DirectUploadEnabled() {
		return nil, ErrEdgeDisabled
	}

	if existing, ok, err := s.Check(ctx, plainHash); err != nil {
		return nil, err
	} else if ok {
		return &IntentResponse{Exists: true, File: existing}, nil
	}

	storageKey := hex.EncodeToString(cipherHash)
	partCount := PartCountFor(size)

	var r2UploadID string
	if partCount > 1 {
		id, err := s.storage.InitMultipart(ctx, storageKey, mime)
		if err != nil {
			return nil, err
		}
		r2UploadID = id
	}

	expiresAt := time.Now().Add(intentTTL)
	it, err := s.repo.InsertIntent(ctx, &UploadIntent{
		UserID: userID, PlainHash: plainHash, CipherHash: cipherHash,
		SizeBytes: size, Mime: mime, StorageKey: storageKey,
		R2UploadID: r2UploadID, PartSize: PartSize, PartCount: partCount,
		ExpiresAt: expiresAt,
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
		SizeBytes:  size,
		PartSize:   PartSize,
		PartCount:  partCount,
		CipherHash: hex.EncodeToString(cipherHash),
		ExpiresAt:  expiresAt.Unix(),
	})
	if err != nil {
		return nil, err
	}

	return &IntentResponse{
		Exists:   false,
		IntentID: it.ID.String(),
		// 空串 = 用 api 自身的 /edge（dev）。客户端据此拼 baseUrl，
		// 于是两种环境共用同一条上传代码路径，只是终点不同。
		UploadURL: s.edgeBase,
		Token:     token,
		PartSize:  PartSize,
		PartCount: partCount,
	}, nil
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

	// 直传路径不生成缩略图：api 手里没有字节，要生成就得把整个对象从 R2 拉回来，
	// 恰好抵消掉直传省下的带宽。而客户端混淆后的字节本来就解不出图（见 Upload 里的说明），
	// 缩略图对新数据一直是 NULL —— 也就是说这里没有任何行为损失。
	return f, nil
}

// abortIntentObject 丢弃意图对应的 R2 半成品：multipart 走 abort，单片走 delete。
// 两者都是尽力而为，失败只记日志 —— 留下的分片是可计费的 dust，但比中断收尾要好。
func (s *Service) abortIntentObject(ctx context.Context, it *UploadIntent) {
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
	if !s.DirectUploadEnabled() {
		slog.Info("未启用直传，跳过上传意图清理循环")
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

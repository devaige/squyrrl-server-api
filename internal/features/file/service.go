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

	"github.com/squyrrl/api/internal/features/file/thumb"
	"github.com/squyrrl/api/internal/infra/storage"
)

type Service struct {
	repo    *Repo
	storage *storage.Client
}

func NewService(repo *Repo, st *storage.Client) *Service {
	return &Service{repo: repo, storage: st}
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

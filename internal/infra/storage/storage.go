package storage

import (
	"context"
	"io"
	"log/slog"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// Client 封装 S3 协议的对象存储（dev 接 MinIO，生产接 R2）
type Client struct {
	mc     *minio.Client
	core   *minio.Core
	bucket string
}

func New(endpoint, accessKey, secretKey, bucket string, useSSL bool) (*Client, error) {
	mc, err := minio.New(endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(accessKey, secretKey, ""),
		Secure: useSSL,
	})
	if err != nil {
		return nil, err
	}
	// Core 暴露未包装的 multipart 原语。直传（ADR-069）里 api 只做 init/complete/abort，
	// 字节由边缘 Worker 写入，因此高层的 PutObject 帮不上忙 —— 它会自己管理分片。
	return &Client{mc: mc, core: &minio.Core{Client: mc}, bucket: bucket}, nil
}

// EnsureBucket 在启动时确保 bucket 存在，dev 环境下首次启动 MinIO 自动建桶
func (c *Client) EnsureBucket(ctx context.Context) error {
	exists, err := c.mc.BucketExists(ctx, c.bucket)
	if err != nil {
		return err
	}
	if exists {
		return nil
	}
	if err := c.mc.MakeBucket(ctx, c.bucket, minio.MakeBucketOptions{}); err != nil {
		return err
	}
	slog.Info("已创建对象存储 bucket", "bucket", c.bucket)
	return nil
}

// Put 上传字节；contentType 不影响存储，仅作元数据
func (c *Client) Put(ctx context.Context, key string, r io.Reader, size int64, contentType string) error {
	_, err := c.mc.PutObject(ctx, c.bucket, key, r, size, minio.PutObjectOptions{
		ContentType: contentType,
	})
	return err
}

// Get 返回流式 reader；caller 负责 Close
func (c *Client) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	obj, err := c.mc.GetObject(ctx, c.bucket, key, minio.GetObjectOptions{})
	if err != nil {
		return nil, err
	}
	// minio.Object 同时实现 io.ReadCloser；调用方需要 Close
	return obj, nil
}

// Delete 删除对象，由 file GC 调用（当 ref_count 归零）
func (c *Client) Delete(ctx context.Context, key string) error {
	return c.mc.RemoveObject(ctx, c.bucket, key, minio.RemoveObjectOptions{})
}

func (c *Client) Bucket() string { return c.bucket }

// =============================================================================
// Multipart：客户端直传（ADR-069）用。api 只签发与收尾，字节流经边缘 Worker，
// 不经过本进程 —— 这正是直传要省下的那部分带宽。
// =============================================================================

// InitMultipart 开一个 multipart 上传，返回 R2 侧的 uploadID
func (c *Client) InitMultipart(ctx context.Context, key, contentType string) (string, error) {
	return c.core.NewMultipartUpload(ctx, c.bucket, key,
		minio.PutObjectOptions{ContentType: contentType})
}

// CompletePart 是一片上传完成后由 Worker 回传的 (序号, ETag)
type CompletePart struct {
	PartNumber int
	ETag       string
}

// CompleteMultipart 合并所有分片。parts 必须按 PartNumber 升序且无缺号，
// 校验放在调用方（service）——那里才有意图表可比对。
func (c *Client) CompleteMultipart(ctx context.Context, key, uploadID string, parts []CompletePart) error {
	ps := make([]minio.CompletePart, len(parts))
	for i, p := range parts {
		ps[i] = minio.CompletePart{PartNumber: p.PartNumber, ETag: p.ETag}
	}
	_, err := c.core.CompleteMultipartUpload(ctx, c.bucket, key, uploadID, ps, minio.PutObjectOptions{})
	return err
}

// AbortMultipart 丢弃未完成的 multipart。已写入的分片在 R2 侧仍然计费，
// 所以过期意图必须显式 abort，不能只删 DB 行。
func (c *Client) AbortMultipart(ctx context.Context, key, uploadID string) error {
	return c.core.AbortMultipartUpload(ctx, c.bucket, key, uploadID)
}

// StatObject 取对象元信息。commit 时用它确认「客户端声称传完了」确有其事，
// 并拿到 R2 记录的真实大小 —— 配额结算必须按这个值，而不是客户端声称的值。
func (c *Client) StatObject(ctx context.Context, key string) (int64, error) {
	info, err := c.mc.StatObject(ctx, c.bucket, key, minio.StatObjectOptions{})
	if err != nil {
		return 0, err
	}
	return info.Size, nil
}

// PutPart 上传一片，返回 ETag。仅由 dev 的内嵌边缘使用 —— 生产由 Cloudflare Worker
// 经 R2 binding 直写，字节根本不进这个进程。
func (c *Client) PutPart(ctx context.Context, key, uploadID string, partNumber int,
	r io.Reader, size int64) (string, error) {
	p, err := c.core.PutObjectPart(ctx, c.bucket, key, uploadID, partNumber, r, size,
		minio.PutObjectPartOptions{})
	if err != nil {
		return "", err
	}
	return p.ETag, nil
}

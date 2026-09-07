package file

import (
	"errors"
	"time"

	"github.com/google/uuid"
)

var (
	ErrNotFound       = errors.New("file not found")
	ErrCipherMismatch = errors.New("cipher hash does not match uploaded bytes")
	ErrSizeMismatch   = errors.New("declared size does not match uploaded bytes")
	ErrNoThumbnail    = errors.New("file has no thumbnail")
	ErrIntentNotFound = errors.New("upload intent not found")
	ErrIntentState    = errors.New("upload intent is not pending")
	ErrPartsMismatch  = errors.New("committed parts do not match the intent")
	ErrEdgeDisabled   = errors.New("direct upload is not configured")
)

// 上传体积上限：100 MiB；后续按订阅档位放宽。
//
// 直传（ADR-069）之后这不再是传输层限制，只是产品策略：字节按 PartSize 分片经边缘
// Worker 写入，单个 HTTP 请求体永远只有一片大小。放宽这个常量不需要动任何基础设施。
const MaxUploadBytes = 100 * 1024 * 1024

// PartSize 是直传的分片大小。
//
// 下界由 S3/R2 定死：multipart 除最后一片外每片至少 5 MiB。上界是 Cloudflare 边缘的
// 请求体限制，它跟**账户 plan** 走而非 Workers plan —— Free/Pro 100 MB、Business 200 MB。
// 取 8 MiB 是在「请求数（每片一次 Class A 计费）」和「单片重传代价」之间折中：
// 100 MiB 文件 13 片，即便撞上 Free plan 的 100 MB 上限也有一个数量级的余量。
const PartSize int64 = 8 * 1024 * 1024

// intentTTL 是上传令牌与意图行的存活时间。
// 必须显著长于最慢的预期上传（弱网下 100 MiB 可能要几十分钟），
// 又必须短到让 sweeper 能及时回收 R2 里的半成品分片。
const intentTTL = 2 * time.Hour

// PartCountFor 按 PartSize 切分；size <= PartSize 时返回 1（走单片模式，
// 省掉 multipart 的 create/complete 两次额外 Class A 操作）。
func PartCountFor(size int64) int {
	if size <= PartSize {
		return 1
	}
	return int((size + PartSize - 1) / PartSize)
}

// UploadIntent 是一次已签发、尚未收尾的直传
type UploadIntent struct {
	ID         uuid.UUID
	UserID     uuid.UUID
	PlainHash  []byte
	CipherHash []byte
	SizeBytes  int64
	Mime       string
	StorageKey string
	R2UploadID string // 空 = 单片模式
	PartSize   int64
	PartCount  int
	Status     string
	ExpiresAt  time.Time
}

// IntentResponse 是 POST /files/intent 的回包。
// exists=true 时其余字段全为零值：客户端命中全局去重，一个字节都不用传。
type IntentResponse struct {
	Exists    bool   `json:"exists"`
	File      *File  `json:"file,omitempty"`
	IntentID  string `json:"intent_id,omitempty"`
	UploadURL string `json:"upload_url,omitempty"`
	Token     string `json:"token,omitempty"`
	PartSize  int64  `json:"part_size,omitempty"`
	PartCount int    `json:"part_count,omitempty"`
}

type IntentInput struct {
	PlainHash  string `json:"plain_hash"  binding:"required,hexadecimal,len=64"`
	CipherHash string `json:"cipher_hash" binding:"required,hexadecimal,len=64"`
	SizeBytes  int64  `json:"size_bytes"  binding:"required,gt=0"`
	Mime       string `json:"mime"`
}

// CommitInput 收尾。单片模式 parts 为空；multipart 模式必须交齐每片的 ETag，
// 顺序无所谓（服务端排序），但序号必须 1..part_count 无缺号。
type CommitInput struct {
	IntentID string       `json:"intent_id" binding:"required,uuid"`
	Parts    []CommitPart `json:"parts"`
}

type CommitPart struct {
	PartNumber int    `json:"part_number" binding:"required,gt=0"`
	ETag       string `json:"etag"        binding:"required"`
}

type File struct {
	ID           uuid.UUID `json:"id"`
	PlainHash    string    `json:"plain_hash"`  // hex(sha256)
	CipherHash   string    `json:"cipher_hash"` // hex(sha256)
	SizeBytes    int64     `json:"size_bytes"`
	Mime         string    `json:"mime"`
	HasThumbnail bool      `json:"has_thumbnail"`
	CreatedAt    time.Time `json:"created_at"`

	// 不输出给客户端
	StorageBucket string `json:"-"`
	StorageKey    string `json:"-"`
	ThumbnailKey  string `json:"-"`
	RefCount      int    `json:"-"`
}

type CheckInput struct {
	PlainHash string `json:"plain_hash" binding:"required,hexadecimal,len=64"`
}

type CheckResponse struct {
	Exists bool  `json:"exists"`
	File   *File `json:"file"`
}

type UploadResponse struct {
	File *File `json:"file"`
}

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
)

// 上传体积上限：100 MiB（per-request）；后续按订阅档位放宽
const MaxUploadBytes = 100 * 1024 * 1024

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

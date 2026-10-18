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
	ErrIntentNotFound = errors.New("upload intent not found")
	ErrIntentState    = errors.New("upload intent is not pending")
	ErrPartsMismatch  = errors.New("committed parts do not match the intent")
	ErrEdgeDisabled   = errors.New("edge is not configured")
	ErrThumbMime      = errors.New("thumbnail declared for an upload that is neither image nor video")
	ErrThumbTooLarge  = errors.New("thumbnail must be under the ceiling and smaller than the blob")
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

// downloadTTL 是下载令牌的存活时间。
//
// 比 intentTTL 短两个数量级：下载票据在客户端真要读字节时才签发，签完立刻用，
// 没有「弱网下传几十分钟」那种长尾。短 TTL 的代价只是「元数据缓存久了要重签」，
// 收益是票据一旦随日志/截图外泄，可用窗口只有几分钟。
const downloadTTL = 10 * time.Minute

// intentTTL 是上传令牌与意图行的存活时间，续期（RenewIntent）每次再给这么久。
// 必须显著长于两次续期之间的间隔（客户端每传一片前看一眼，弱网下一片 8 MiB 也就几分钟），
// 又必须短到让 sweeper 能及时回收 R2 里的半成品分片，并让没传完的上传别把配额
// 预留太久（heldBytes 只算未过期的 pending）。客户端停下超过这么久，这个文件从头再传。
const intentTTL = 2 * time.Hour

// MaxThumbBytes 是客户端缩略图（**混淆后**）的字节上限。
//
// 客户端按「长边取原图 1/3、封顶 512 px」生成，超了就逐级降 JPEG 质量直到装下，
// 实在装不下就不带缩略图；混淆只加 25 字节头加每块 16 字节 tag。
//
// 它必须存在，是因为缩略图字节**不计存储配额**（理由见 validateThumb）：没有天花板
// 的话，一个 1 字节的本体就能拖着一份任意大的「缩略图」进 R2，而配额只看得见那 1 字节。
const MaxThumbBytes int64 = 64 * 1024

// MaxUpstreamThumbBytes 是上游（TG 等）自带缩略图的字节上限（**混淆后**）。
//
// 比 MaxThumbBytes 宽，是因为这份字节不是我们压的：TG 照片的中间档常见 800 px、
// 60–150 KB，卡 64 KiB 就只能退到 320 px 甚至 90 px 那档（2026-10-14 用户决策）。
// 放宽不会让未计费面积失控 —— 真正框住它的是「必须严格小于本体」那条，见 validateThumb。
const MaxUpstreamThumbBytes int64 = 256 * 1024

// MaxDimension 是声明尺寸的合理上限。尺寸只用于排版，越界的声明按「未知」处理而不是
// 拒绝整次上传 —— 与缩略图同一个原则：排版信息出问题，不能拖垮一份本该成功的上传。
const MaxDimension = 1 << 16

// Dims 是一张图的像素尺寸。零值 = 未知（老数据、非图片、上传方没给）。
type Dims struct {
	Width  int
	Height int
}

// NormDims 把声明的尺寸规整成 Dims：任一边不合法就整体视为未知，
// 让「只知道宽不知道高」这种半状态不可表示（与 chk_files_dims 一致）。
func NormDims(w, h int) Dims {
	if w <= 0 || h <= 0 || w > MaxDimension || h > MaxDimension {
		return Dims{}
	}
	return Dims{Width: w, Height: h}
}

// nullable 给 SQL 用：未知写 NULL，而不是 0 —— 0 会被 CHECK 拒掉。
func (d Dims) nullable() (*int, *int) {
	if d.Width <= 0 || d.Height <= 0 {
		return nil, nil
	}
	w, h := d.Width, d.Height
	return &w, &h
}

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

	// 缩略图的声明。空 = 本次不带缩略图，这是完全正常的状态（非图片、生成失败、
	// 老客户端），下载侧从第一天起就支持 thumb_url 缺省。
	ThumbCipherHash []byte
	ThumbSizeBytes  int64

	// 声明的原图与缩略图尺寸，收尾时原样落进 files。
	Dims      Dims
	ThumbDims Dims
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

	// ThumbToken 是缩略图专用的第二张上传票，恒为单片模式，key 签死在
	// thumb/<本体 cipher_hash>.jpg。缺省即服务端没有接受这次缩略图声明
	// （非图片 / 超限 / 压根没声明），客户端据此跳过那一次 PUT。
	//
	// 单独一张票而不是给本体那张加字段：边缘凭「票里的 key」决定写哪个对象，
	// 一张票只能写一个 key 是这套令牌最要紧的不变式（见 token.go）。
	ThumbToken string `json:"thumb_token,omitempty"`

	// ExpiresAt 是两张票的过期时刻（Unix 秒）。客户端据此在过期前续期，
	// 而不是撞上边缘的 401 才知道。
	ExpiresAt int64 `json:"expires_at,omitempty"`
}

// RenewResponse 是 POST /files/intent/{id}/renew 的回包：同一个意图、同一组已传分片，
// 换一组新票。意图其余的东西（分片大小、片数、upload_url）都没变，不再重复下发。
type RenewResponse struct {
	Token      string `json:"token"`
	ThumbToken string `json:"thumb_token,omitempty"`
	ExpiresAt  int64  `json:"expires_at"`
}

type IntentInput struct {
	PlainHash  string `json:"plain_hash"  binding:"required,hexadecimal,len=64"`
	CipherHash string `json:"cipher_hash" binding:"required,hexadecimal,len=64"`
	SizeBytes  int64  `json:"size_bytes"  binding:"required,gt=0"`
	Mime       string `json:"mime"`

	// 缩略图可选。omitempty 而非 required：不带缩略图是合法上传，
	// 而且必须一直合法 —— 非图片、解码失败、老版本客户端都会走到这里。
	ThumbCipherHash string `json:"thumb_cipher_hash" binding:"omitempty,hexadecimal,len=64"`
	ThumbSizeBytes  int64  `json:"thumb_size_bytes"`

	// 尺寸全部可选，越界按未知处理（见 NormDims）。thumb_* 只在声明了缩略图时才有意义。
	Width       int `json:"width"`
	Height      int `json:"height"`
	ThumbWidth  int `json:"thumb_width"`
	ThumbHeight int `json:"thumb_height"`
}

// IntentRequest 是 IssueIntent 的入参。
//
// 收成结构体而不是继续加位置参数：加上缩略图之后有三个 []byte（两个哈希加一个）
// 和两个 int64（两个大小）互相紧挨着，位置调用里写反一对不会有任何编译错误，
// 而表现是「对象写到了别人的 key 上」。字段名让这类错误在阅读时就露出来。
type IntentRequest struct {
	UserID     uuid.UUID
	PlainHash  []byte
	CipherHash []byte
	SizeBytes  int64
	Mime       string

	// 两项同进同退：要么都给，要么都不给（与 upload_intents 的 CHECK 一致）。
	ThumbCipherHash []byte
	ThumbSizeBytes  int64

	Dims      Dims
	ThumbDims Dims
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

	// 像素尺寸，0 = 未知（缺省不输出）。缩略图尺寸只在 HasThumbnail 时可能有值。
	Width       int `json:"width,omitempty"`
	Height      int `json:"height,omitempty"`
	ThumbWidth  int `json:"thumb_width,omitempty"`
	ThumbHeight int `json:"thumb_height,omitempty"`

	// 不输出给客户端
	StorageBucket string `json:"-"`
	StorageKey    string `json:"-"`
	ThumbnailKey  string `json:"-"`
	RefCount      int    `json:"-"`
}

// TicketResponse 是 GET /files/{id}/ticket 的回包：文件元数据 + 一组短期边缘直读 URL。
//
// 把元数据一起回来是为了省一次往返：调用方（附件预览）本来就要先拿 mime 才知道
// 该读缩略图还是原图。ThumbURL 为空即「没有缩略图」—— 旧接口是让客户端打一次
// /thumb 吃 404 才知道，那一次 404 现在被这个字段消掉了。
type TicketResponse struct {
	File      *File  `json:"file"`
	BlobURL   string `json:"blob_url"`
	ThumbURL  string `json:"thumb_url,omitempty"`
	ExpiresAt int64  `json:"expires_at"`
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

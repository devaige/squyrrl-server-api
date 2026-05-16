package archive

import (
	"errors"

	"github.com/google/uuid"
)

var (
	ErrNotURLSnippet = errors.New("snippet is not a URL type")
	ErrNoURL         = errors.New("snippet payload lacks a usable url field")
	ErrTooLarge      = errors.New("archived content exceeds size limit")
	ErrNoRenderer    = errors.New("no renderer available")
)

// 单次 archive 最大字节数：5 MiB。超过则拒绝（避免内存爆）。
// chromedp renderer 走 ChromeMaxBytes（20 MiB），见 chrome_renderer.go
const MaxArchiveBytes = 5 * 1024 * 1024

type ArchiveResponse struct {
	SnippetID uuid.UUID `json:"snippet_id"`
	FileID    uuid.UUID `json:"file_id"`
	ElementID uuid.UUID `json:"element_id"`
	SizeBytes int64     `json:"size_bytes"`
	Mime      string    `json:"mime"`
	Renderer  string    `json:"renderer"` // 'light' / 'chromedp'
}

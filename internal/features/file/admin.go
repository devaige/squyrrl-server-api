package file

import (
	"context"
	"encoding/hex"
	"strconv"
	"strings"

	"github.com/google/uuid"
)

// 后台文件列表（2026-10-14，用户决策）：全局列出 files 行及其引用次数。只读，只挂在 /internal/admin 下。
//
// ref_count 对客户端不输出（File.RefCount 是 json:"-"）：全局去重（ADR-004）下它等于
// 「还有多少条附件指着同一份字节」，含其他用户的，属于跨用户信息。后台要它看去重效果和
// GC 状态（0 = 没有引用，过了豁免期就会被 file GC 回收），所以这里单独包一层输出。

const (
	AdminDefaultLimit = 50
	AdminMaxLimit     = 100
)

type AdminListInput struct {
	// 空 = 不过滤；完整 UUID = 按文件 id；64 位 hex = 按 plain_hash 或 cipher_hash；
	// 其余 = mime 前缀（image/、video/mp4 …），不区分大小写。
	Q string `form:"q"`
	// created（默认，最新的在前）| refs（引用次数多的在前）| size（大的在前）
	Sort   string `form:"sort" binding:"omitempty,oneof=created refs size"`
	Limit  int    `form:"limit"`
	Offset int    `form:"offset"`
}

// AdminFile 在客户端那份 File 上补出 ref_count。
type AdminFile struct {
	File
	RefCount int `json:"ref_count"`
}

type AdminListOutput struct {
	Items []AdminFile `json:"items"`
	// 符合条件的总条数，items 只是当前这一页。
	Total int `json:"total"`
}

// 每种排序都以 id 收尾：同值的行在翻页之间顺序稳定，不会一页重复、一页漏掉。
var adminSortSQL = map[string]string{
	"":        "created_at DESC, id",
	"created": "created_at DESC, id",
	"refs":    "ref_count DESC, created_at DESC, id",
	"size":    "size_bytes DESC, id",
}

func clampAdminLimit(n int) int {
	if n <= 0 {
		return AdminDefaultLimit
	}
	return min(n, AdminMaxLimit)
}

// adminFilter 把 q 翻译成 WHERE 子句与参数；q 为空返回 "TRUE"。
func adminFilter(q string) (string, []any) {
	q = strings.TrimSpace(q)
	if q == "" {
		return "TRUE", nil
	}
	if id, err := uuid.Parse(q); err == nil {
		return "id = $1", []any{id}
	}
	if len(q) == 64 {
		if h, err := hex.DecodeString(q); err == nil {
			return "(plain_hash = $1 OR cipher_hash = $1)", []any{h}
		}
	}
	// 反斜杠是 Postgres LIKE 的默认转义符；不转义的话 `_` 会匹配任意单个字符。
	esc := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(q)
	return "mime ILIKE $1", []any{esc + "%"}
}

// AdminList 分页列出文件。用 offset 而不是游标：后台要的是「翻到第几页」。
func (r *Repo) AdminList(ctx context.Context, in AdminListInput) (*AdminListOutput, error) {
	where, args := adminFilter(in.Q)
	out := &AdminListOutput{Items: []AdminFile{}}
	if err := r.pool.QueryRow(ctx, `SELECT count(*) FROM files WHERE `+where, args...).
		Scan(&out.Total); err != nil {
		return nil, err
	}

	n := len(args)
	rows, err := r.pool.Query(ctx, `
		SELECT `+fileCols+`
		FROM files
		WHERE `+where+`
		ORDER BY `+adminSortSQL[in.Sort]+`
		LIMIT $`+strconv.Itoa(n+1)+` OFFSET $`+strconv.Itoa(n+2),
		append(args, clampAdminLimit(in.Limit), max(in.Offset, 0))...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		f, err := scanFile(rows)
		if err != nil {
			return nil, err
		}
		out.Items = append(out.Items, AdminFile{File: *f, RefCount: f.RefCount})
	}
	return out, rows.Err()
}

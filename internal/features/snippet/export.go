package snippet

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// 导出信封格式。与客户端的本地备份（client/flutter 的 local_backup.dart）**共用同一个
// format 标识**，因为这正是它的用途：降级用户把云端数据导出来，直接导回本地库继续用。
// 两边各写一份解析器，格式契约记在 docs/spec/backup-envelope.md。
//
// 版本 2 而不是复用 1：服务端导出的 text_content 是**混淆后的字节**，
// 而 v1（客户端自己写的本地备份）里是明文。同一个字段两种含义，
// 只能靠版本号区分 —— 让旧客户端读到 v2 时明确报错，
// 好过让它把密文当明文导进去，得到一库乱码。
const (
	ExportFormat  = "squyrrl.local-backup"
	ExportVersion = 2
)

// Exporter 把一个用户的云端数据流式写出。
//
// 流式而不是先拼成一个大对象：至尊版的上限是 1000 万条碎片，
// 任何「先 append 到切片再 Marshal」的写法都会在这个量级上把进程打爆。
// 这里全程只持有一行的内存。
type Exporter struct {
	pool *pgxpool.Pool
}

func NewExporter(pool *pgxpool.Pool) *Exporter { return &Exporter{pool: pool} }

// Export 写出该用户的页面与碎片。
//
// **冻结期的碎片不导出**：那一阶段的定义就是「不可访问」（ADR-075，2026-09-11 用户决策），
// 导出是访问的一种，放行等于让冻结期形同虚设。宽限期内的照常导出 ——
// 那 30 天存在的理由正是让用户把东西拿走。
func (e *Exporter) Export(ctx context.Context, userID uuid.UUID, w io.Writer) error {
	if _, err := fmt.Fprintf(w,
		`{"format":%q,"version":%d,"exported_at":%q,"content_obfuscated":true,"pages":[`,
		ExportFormat, ExportVersion, time.Now().UTC().Format(time.RFC3339)); err != nil {
		return err
	}
	if err := e.writePages(ctx, userID, w); err != nil {
		return err
	}
	if _, err := io.WriteString(w, `],"snippets":[`); err != nil {
		return err
	}
	if err := e.writeSnippets(ctx, userID, w); err != nil {
		return err
	}
	_, err := io.WriteString(w, `]}`)
	return err
}

func (e *Exporter) writePages(ctx context.Context, userID uuid.UUID, w io.Writer) error {
	rows, err := e.pool.Query(ctx, `
		SELECT id, name, is_hidden, is_system, order_idx
		FROM pages WHERE user_id = $1 AND deleted_at IS NULL ORDER BY order_idx, id`, userID)
	if err != nil {
		return err
	}
	defer rows.Close()

	enc := json.NewEncoder(w)
	first := true
	for rows.Next() {
		var p struct {
			ID       uuid.UUID `json:"id"`
			Name     string    `json:"name"`
			IsHidden bool      `json:"is_hidden"`
			IsSystem bool      `json:"is_system"`
			OrderIdx int       `json:"order_idx"`
		}
		if err := rows.Scan(&p.ID, &p.Name, &p.IsHidden, &p.IsSystem, &p.OrderIdx); err != nil {
			return err
		}
		if err := writeSep(w, &first); err != nil {
			return err
		}
		if err := enc.Encode(p); err != nil {
			return err
		}
	}
	return rows.Err()
}

func (e *Exporter) writeSnippets(ctx context.Context, userID uuid.UUID, w io.Writer) error {
	// restricted_at 的判据与 RestrictionOf 一致：只排除**已进入冻结期**的，
	// 宽限期内的照常导出。两处若分叉，用户会在导出里拿到一批他在界面上打不开的东西
	// —— 或者反过来，界面说能看的导不出来。
	rows, err := e.pool.Query(ctx, `
		SELECT id, page_id, type, subtype, title, description,
		       text_content, text_format, text_lang, payload,
		       source_type, source_data, version, created_at, updated_at,
		       COALESCE((SELECT array_agg(tag_id) FROM snippet_tags WHERE snippet_id = s.id), '{}')
		FROM snippets s
		WHERE user_id = $1 AND deleted_at IS NULL
		  AND (restricted_at IS NULL OR restricted_at + $2::interval > now())
		ORDER BY created_at, id`,
		userID, RestrictionGrace.String())
	if err != nil {
		return err
	}
	defer rows.Close()

	enc := json.NewEncoder(w)
	first := true
	for rows.Next() {
		var (
			id                             uuid.UUID
			pageID                         *uuid.UUID
			typ, textFormat, sourceType    string
			subtype, title, desc, textLang *string
			textContent                    []byte
			payload, sourceData            json.RawMessage
			version                        int64
			createdAt, updatedAt           time.Time
			tagIDs                         []uuid.UUID
		)
		if err := rows.Scan(&id, &pageID, &typ, &subtype, &title, &desc,
			&textContent, &textFormat, &textLang, &payload,
			&sourceType, &sourceData, &version, &createdAt, &updatedAt, &tagIDs); err != nil {
			return err
		}
		out := map[string]any{
			"id":          id,
			"page_id":     pageID,
			"type":        typ,
			"subtype":     subtype,
			"title":       title,
			"description": desc,
			"text_format": textFormat,
			"text_lang":   textLang,
			"payload":     rawOrEmpty(payload),
			"source_type": sourceType,
			"source_data": rawOrEmpty(sourceData),
			"version":     version,
			"tag_ids":     tagIDs,
			"created_at":  createdAt,
			"updated_at":  updatedAt,
		}
		// base64 手动编码而不是靠 []byte 的默认行为：默认行为确实也是 base64，
		// 但把它写出来才说得清「这里是密文、需要客户端解混淆」这件事。
		if len(textContent) > 0 {
			out["text_content"] = base64.StdEncoding.EncodeToString(textContent)
		}
		if err := writeSep(w, &first); err != nil {
			return err
		}
		if err := enc.Encode(out); err != nil {
			return err
		}
	}
	return rows.Err()
}

// writeSep 在第一项之外的每项前写逗号。json.Encoder.Encode 会自带换行，
// 所以产出是「一行一条」的紧凑数组，既合法又便于人工排查。
func writeSep(w io.Writer, first *bool) error {
	if *first {
		*first = false
		return nil
	}
	_, err := io.WriteString(w, ",")
	return err
}

func rawOrEmpty(r json.RawMessage) json.RawMessage {
	if len(r) == 0 {
		return json.RawMessage("{}")
	}
	return r
}

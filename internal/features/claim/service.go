package claim

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Service struct {
	pool *pgxpool.Pool
}

func NewService(pool *pgxpool.Pool) *Service { return &Service{pool: pool} }

// Apply 在单一事务里把 in 全部内容写入 userID 名下。
// 流程：
//  1. 校验大小上限（防滥用）
//  2. 查 anonymous_claims：命中即幂等返回，不再写
//  3. 开事务：先 pages → 再 tags（带 name 冲突 remap）→ 再 snippets → 再 elements + snippet_tags
//  4. 写 anonymous_claims 记录本次完成
func (s *Service) Apply(ctx context.Context, userID uuid.UUID, in *ClaimInput) (*ClaimResult, error) {
	if err := validateLimits(in); err != nil {
		return nil, err
	}

	// 幂等检查（事务外快路径）
	if prev, ok, err := s.lookupExistingClaim(ctx, userID, in.ClientDedupeKey); err != nil {
		return nil, err
	} else if ok {
		prev.Idempotent = true
		return prev, nil
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)

	// 再次检查（事务内防并发重放）；如果别的请求刚好提交了同一个 key，直接退化为幂等
	if prev, ok, err := lookupExistingClaimTx(ctx, tx, userID, in.ClientDedupeKey); err != nil {
		return nil, err
	} else if ok {
		prev.Idempotent = true
		return prev, nil
	}

	pageCount, err := insertPages(ctx, tx, userID, in.Pages)
	if err != nil {
		return nil, err
	}

	// 标签先批量插入：name 与用户已有标签冲突时，复用已有 row，build tagRemap
	tagCount, tagRemap, err := insertTags(ctx, tx, userID, in.Tags)
	if err != nil {
		return nil, err
	}

	snippetCount, elementCount, err := insertSnippets(ctx, tx, userID, in.Snippets, tagRemap)
	if err != nil {
		return nil, err
	}

	result := &ClaimResult{
		ClientDedupeKey: in.ClientDedupeKey,
		UserID:          userID,
		SnippetCount:    snippetCount,
		PageCount:       pageCount,
		TagCount:        tagCount,
		ElementCount:    elementCount,
	}

	if _, err := tx.Exec(ctx, `
		INSERT INTO anonymous_claims (id, user_id, snippet_count, page_count, tag_count, element_count)
		VALUES ($1, $2, $3, $4, $5, $6)`,
		in.ClientDedupeKey, userID, snippetCount, pageCount, tagCount, elementCount,
	); err != nil {
		return nil, err
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return result, nil
}

func validateLimits(in *ClaimInput) error {
	if len(in.Pages) > MaxPages {
		return ErrPayloadTooLarge
	}
	if len(in.Tags) > MaxTags {
		return ErrPayloadTooLarge
	}
	if len(in.Snippets) > MaxSnippets {
		return ErrPayloadTooLarge
	}
	elementTotal := 0
	for _, s := range in.Snippets {
		elementTotal += len(s.Elements)
	}
	if elementTotal > MaxElements {
		return ErrPayloadTooLarge
	}
	return nil
}

func (s *Service) lookupExistingClaim(ctx context.Context, userID, key uuid.UUID) (*ClaimResult, bool, error) {
	row := s.pool.QueryRow(ctx, `
		SELECT snippet_count, page_count, tag_count, element_count
		FROM anonymous_claims
		WHERE id = $1 AND user_id = $2`,
		key, userID)
	var r ClaimResult
	if err := row.Scan(&r.SnippetCount, &r.PageCount, &r.TagCount, &r.ElementCount); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, false, nil
		}
		return nil, false, err
	}
	r.ClientDedupeKey = key
	r.UserID = userID
	return &r, true, nil
}

func lookupExistingClaimTx(ctx context.Context, tx pgx.Tx, userID, key uuid.UUID) (*ClaimResult, bool, error) {
	row := tx.QueryRow(ctx, `
		SELECT snippet_count, page_count, tag_count, element_count
		FROM anonymous_claims
		WHERE id = $1 AND user_id = $2
		FOR UPDATE`,
		key, userID)
	var r ClaimResult
	if err := row.Scan(&r.SnippetCount, &r.PageCount, &r.TagCount, &r.ElementCount); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, false, nil
		}
		return nil, false, err
	}
	r.ClientDedupeKey = key
	r.UserID = userID
	return &r, true, nil
}

// insertPages 用 COPY 批量写。客户端 UUID 与已有 page 冲突时 DO NOTHING（极小概率），
// 不视为错误：相同 ID 通常意味着该客户端已经 claim 过又重试，且新 row 被覆盖前的脏数据。
func insertPages(ctx context.Context, tx pgx.Tx, userID uuid.UUID, pages []PageInput) (int, error) {
	if len(pages) == 0 {
		return 0, nil
	}
	inserted := 0
	// 用单条 INSERT ... VALUES (...) ON CONFLICT DO NOTHING 一次落表
	// COPY 不支持 ON CONFLICT，所以批 200 一组拼参数化 SQL
	const batch = 200
	for start := 0; start < len(pages); start += batch {
		end := start + batch
		if end > len(pages) {
			end = len(pages)
		}
		chunk := pages[start:end]

		args := make([]any, 0, len(chunk)*5)
		var sb stringBuilder
		sb.WriteString(`INSERT INTO pages (id, user_id, name, is_hidden, order_idx) VALUES `)
		for i, p := range chunk {
			if i > 0 {
				sb.WriteString(",")
			}
			base := i * 5
			sb.WriteString("($")
			sb.WriteIntPlus(base + 1)
			sb.WriteString(",$")
			sb.WriteIntPlus(base + 2)
			sb.WriteString(",$")
			sb.WriteIntPlus(base + 3)
			sb.WriteString(",$")
			sb.WriteIntPlus(base + 4)
			sb.WriteString(",$")
			sb.WriteIntPlus(base + 5)
			sb.WriteString(")")
			args = append(args, p.ID, userID, p.Name, p.IsHidden, p.OrderIdx)
		}
		sb.WriteString(" ON CONFLICT (id) DO NOTHING")

		tag, err := tx.Exec(ctx, sb.String(), args...)
		if err != nil {
			return 0, err
		}
		inserted += int(tag.RowsAffected())
	}
	return inserted, nil
}

// insertTags 处理标签 name 冲突：
//   - INSERT ... ON CONFLICT (user_id, name) DO NOTHING
//   - 然后 SELECT id FROM tags WHERE user_id=$1 AND name=ANY(...) 把所有相关 tag 实际 id 取回
//   - 客户端 UUID != 实际 UUID 的，写入 remap，让 snippet_tags 用真实 ID
func insertTags(
	ctx context.Context,
	tx pgx.Tx,
	userID uuid.UUID,
	tags []TagInput,
) (int, map[uuid.UUID]uuid.UUID, error) {
	if len(tags) == 0 {
		return 0, nil, nil
	}

	// 同次 claim 内部按 name 去重——客户端可能不慎重复
	seenName := make(map[string]int, len(tags))
	for i, t := range tags {
		if _, ok := seenName[t.Name]; !ok {
			seenName[t.Name] = i
		}
	}

	const batch = 200
	inserted := 0
	for start := 0; start < len(tags); start += batch {
		end := start + batch
		if end > len(tags) {
			end = len(tags)
		}
		chunk := tags[start:end]

		args := make([]any, 0, len(chunk)*3)
		var sb stringBuilder
		sb.WriteString(`INSERT INTO tags (id, user_id, name) VALUES `)
		for i, t := range chunk {
			if i > 0 {
				sb.WriteString(",")
			}
			base := i * 3
			sb.WriteString("($")
			sb.WriteIntPlus(base + 1)
			sb.WriteString(",$")
			sb.WriteIntPlus(base + 2)
			sb.WriteString(",$")
			sb.WriteIntPlus(base + 3)
			sb.WriteString(")")
			args = append(args, t.ID, userID, t.Name)
		}
		sb.WriteString(" ON CONFLICT (user_id, name) DO NOTHING")

		tag, err := tx.Exec(ctx, sb.String(), args...)
		if err != nil {
			return 0, nil, err
		}
		inserted += int(tag.RowsAffected())
	}

	// 取回 name → real_id；用于 build remap
	names := make([]string, 0, len(seenName))
	for n := range seenName {
		names = append(names, n)
	}
	rows, err := tx.Query(ctx, `
		SELECT id, name FROM tags WHERE user_id = $1 AND name = ANY($2)`,
		userID, names)
	if err != nil {
		return 0, nil, err
	}
	defer rows.Close()
	nameToID := make(map[string]uuid.UUID, len(names))
	for rows.Next() {
		var id uuid.UUID
		var name string
		if err := rows.Scan(&id, &name); err != nil {
			return 0, nil, err
		}
		nameToID[name] = id
	}
	if err := rows.Err(); err != nil {
		return 0, nil, err
	}

	remap := make(map[uuid.UUID]uuid.UUID, len(tags))
	for _, t := range tags {
		real, ok := nameToID[t.Name]
		if !ok {
			// 理论不可能：刚 INSERT ON CONFLICT 完，SELECT 必命中
			return 0, nil, ErrDuplicateName
		}
		if real != t.ID {
			remap[t.ID] = real
		}
	}
	return inserted, remap, nil
}

// insertSnippets 写 snippets + elements + snippet_tags 三表；ID 全部来自客户端。
// page_id：必须是本次 claim 新建的 page 或用户已拥有的 page；服务端用 EXISTS 子查询校验，
// 不归属当前用户的引用直接置 NULL（容忍旧设备脏数据）。
func insertSnippets(
	ctx context.Context,
	tx pgx.Tx,
	userID uuid.UUID,
	snippets []SnippetInput,
	tagRemap map[uuid.UUID]uuid.UUID,
) (int, int, error) {
	if len(snippets) == 0 {
		return 0, 0, nil
	}

	// 收集所有 file_id，提前校验存在性（files 表是全局共享，跨用户去重）
	fileIDSet := make(map[uuid.UUID]struct{})
	for _, s := range snippets {
		for _, e := range s.Elements {
			fileIDSet[e.FileID] = struct{}{}
		}
	}
	if len(fileIDSet) > 0 {
		ids := make([]uuid.UUID, 0, len(fileIDSet))
		for id := range fileIDSet {
			ids = append(ids, id)
		}
		var n int
		if err := tx.QueryRow(ctx,
			`SELECT count(*) FROM files WHERE id = ANY($1)`, ids).Scan(&n); err != nil {
			return 0, 0, err
		}
		if n != len(ids) {
			return 0, 0, ErrFileNotFound
		}
	}

	snippetCount := 0
	elementCount := 0

	for _, s := range snippets {
		textFormat := s.TextFormat
		if textFormat == "" {
			textFormat = "plain"
		}
		payload := s.Payload
		if len(payload) == 0 {
			payload = json.RawMessage("{}")
		}
		sourceData := s.SourceData
		if len(sourceData) == 0 {
			sourceData = json.RawMessage("{}")
		}

		// page_id：仅当指向用户名下活跃 page 时才保留；否则置 NULL
		var pageID *uuid.UUID
		if s.PageID != nil {
			var exists bool
			if err := tx.QueryRow(ctx, `
				SELECT EXISTS(SELECT 1 FROM pages WHERE id = $1 AND user_id = $2 AND deleted_at IS NULL)`,
				*s.PageID, userID,
			).Scan(&exists); err != nil {
				return 0, 0, err
			}
			if exists {
				pageID = s.PageID
			}
		}

		tag, err := tx.Exec(ctx, `
			INSERT INTO snippets (
				id, user_id, page_id, type, subtype, title, description,
				text_content, text_format, text_lang, payload,
				source_type, source_data
			) VALUES (
				$1, $2, $3, $4, $5, $6, $7,
				$8, $9, $10, $11,
				$12, $13
			)
			ON CONFLICT (id) DO NOTHING`,
			s.ID, userID, pageID, s.Type, s.Subtype, s.Title, s.Description,
			s.TextContent, textFormat, s.TextLang, payload,
			s.SourceType, sourceData,
		)
		if err != nil {
			return 0, 0, err
		}
		// 即使 PK 冲突跳过（极小概率），也要继续走 tags / elements
		// —— 因为是同一 claim 的内容，如果 snippet 行已存在则它应该已被关联好；这里幂等保守跳过
		if tag.RowsAffected() == 0 {
			continue
		}
		snippetCount++

		// snippet_tags
		if len(s.TagIDs) > 0 {
			rows := make([][]any, 0, len(s.TagIDs))
			seen := make(map[uuid.UUID]struct{}, len(s.TagIDs))
			for _, tid := range s.TagIDs {
				realID := tid
				if mapped, ok := tagRemap[tid]; ok {
					realID = mapped
				}
				if _, dup := seen[realID]; dup {
					continue
				}
				seen[realID] = struct{}{}
				// 防止把不归属当前用户的 tag 关进来：用 SELECT 校验
				var owned bool
				if err := tx.QueryRow(ctx,
					`SELECT EXISTS(SELECT 1 FROM tags WHERE id = $1 AND user_id = $2)`,
					realID, userID,
				).Scan(&owned); err != nil {
					return 0, 0, err
				}
				if !owned {
					continue
				}
				rows = append(rows, []any{s.ID, realID})
			}
			if len(rows) > 0 {
				if _, err := tx.CopyFrom(ctx,
					pgx.Identifier{"snippet_tags"},
					[]string{"snippet_id", "tag_id"},
					pgx.CopyFromRows(rows),
				); err != nil {
					return 0, 0, err
				}
			}
		}

		// elements
		if len(s.Elements) > 0 {
			rows := make([][]any, 0, len(s.Elements))
			for _, e := range s.Elements {
				rows = append(rows, []any{s.ID, e.FileID, e.OrderIdx, e.AltText})
			}
			if _, err := tx.CopyFrom(ctx,
				pgx.Identifier{"elements"},
				[]string{"snippet_id", "file_id", "order_idx", "alt_text"},
				pgx.CopyFromRows(rows),
			); err != nil {
				return 0, 0, err
			}
			elementCount += len(rows)
		}
	}

	slog.DebugContext(ctx, "claim applied",
		"user_id", userID,
		"snippets", snippetCount,
		"elements", elementCount,
	)
	return snippetCount, elementCount, nil
}

// stringBuilder 是极简的 strings.Builder 替代，避免在 hot path 引入 fmt.Sprintf。
// 仅供包内拼参数化 SQL 用。
type stringBuilder struct {
	buf []byte
}

func (b *stringBuilder) WriteString(s string) { b.buf = append(b.buf, s...) }
func (b *stringBuilder) String() string       { return string(b.buf) }
func (b *stringBuilder) WriteIntPlus(n int) {
	if n == 0 {
		b.buf = append(b.buf, '0')
		return
	}
	var tmp [20]byte
	i := len(tmp)
	for n > 0 {
		i--
		tmp[i] = byte('0' + n%10)
		n /= 10
	}
	b.buf = append(b.buf, tmp[i:]...)
}

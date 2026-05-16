package parser

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Cache 封装对 parse_cache 表的存取。同 URI 跨用户复用解析结果。
type Cache struct {
	pool *pgxpool.Pool
}

func NewCache(pool *pgxpool.Pool) *Cache { return &Cache{pool: pool} }

// Get 命中返回 ParsedSnippet（含 type/subtype/title/payload/source_*）；未命中返回 false
func (c *Cache) Get(ctx context.Context, provider, resourceID string) (*ParsedSnippet, bool, error) {
	var raw []byte
	err := c.pool.QueryRow(ctx, `
		SELECT payload FROM parse_cache
		WHERE provider = $1 AND provider_resource_id = $2
		  AND (expires_at IS NULL OR expires_at > now())`,
		provider, resourceID).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	var snip ParsedSnippet
	if err := json.Unmarshal(raw, &snip); err != nil {
		return nil, false, err
	}
	return &snip, true, nil
}

// Put 落库或更新缓存。expires_at 为 nil 表示永不过期。
func (c *Cache) Put(ctx context.Context, provider, resourceID string, snip *ParsedSnippet) error {
	raw, err := json.Marshal(snip)
	if err != nil {
		return err
	}
	_, err = c.pool.Exec(ctx, `
		INSERT INTO parse_cache (provider, provider_resource_id, payload, file_ids)
		VALUES ($1, $2, $3, '{}')
		ON CONFLICT (provider, provider_resource_id) DO UPDATE
			SET payload = EXCLUDED.payload, expires_at = NULL`,
		provider, resourceID, raw)
	return err
}

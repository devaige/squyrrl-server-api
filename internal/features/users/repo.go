package users

import (
	"context"
	"strconv"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Repo struct {
	pool *pgxpool.Pool
}

func NewRepo(pool *pgxpool.Pool) *Repo { return &Repo{pool: pool} }

// Search 按关键字查用户，最近注册的在前；只填账户字段，钱包字段由 Service 补。
//
// q 的解释只有三种：空 = 不过滤；能解析成 UUID = 按 id 精确匹配；其余 = 邮箱子串匹配。
// 不同时匹配 id 前缀：后台拿到的 ID 都是从日志或工单里整段复制的，前缀查询只会多出误命中。
// 软删除的用户与 auth.Repo.GetUser 一致，视为不存在。
func (r *Repo) Search(ctx context.Context, q string, limit int) ([]Summary, error) {
	where := "deleted_at IS NULL"
	var args []any
	if q != "" {
		if id, err := uuid.Parse(q); err == nil {
			args = append(args, id)
			where += " AND id = $1"
		} else {
			args = append(args, containsPattern(q))
			where += " AND email ILIKE $1"
		}
	}
	args = append(args, limit)

	rows, err := r.pool.Query(ctx, `
		SELECT id, email, handle, created_at FROM users
		WHERE `+where+`
		ORDER BY created_at DESC, id DESC
		LIMIT $`+strconv.Itoa(len(args)), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []Summary{}
	for rows.Next() {
		var s Summary
		if err := rows.Scan(&s.ID, &s.Email, &s.Handle, &s.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// containsPattern 把用户输入包成 ILIKE 子串模式，并转义其中的通配符。
//
// 邮箱里 `_` 很常见（john_doe@…），不转义的话它会匹配任意单个字符，
// 搜 a_b 会把 axb 也带出来。反斜杠是 Postgres LIKE 的默认转义符，所以它自己也要转义。
func containsPattern(q string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return "%" + r.Replace(q) + "%"
}

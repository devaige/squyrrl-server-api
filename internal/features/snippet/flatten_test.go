package snippet

import (
	"context"
	"encoding/json"
	"os"
	"testing"

	"github.com/google/uuid"

	"github.com/squyrrl/api/internal/infra/db"
)

func specialSnippet() *Snippet {
	sub := "telegram_message"
	return &Snippet{
		Type:        "special",
		Subtype:     &sub,
		TextContent: []byte("obfuscated"),
		TextFormat:  "markdown",
		Payload:     json.RawMessage(`{"kind":"message","url":"https://t.me/c/1/2"}`),
		SourceType:  "upstream",
		SourceData:  json.RawMessage(`{"provider":"telegram","tg_message_id":2}`),
	}
}

func TestApplyFlattenClearsTypeSpecificColumns(t *testing.T) {
	cur := specialSnippet()
	applyFlatten(cur, &UpdateInput{Flatten: true})

	if cur.Type != "normal" || cur.Subtype != nil {
		t.Errorf("应坍塌为 normal 且清空 subtype，得到 %s/%v", cur.Type, cur.Subtype)
	}
	if string(cur.Payload) != "{}" || string(cur.SourceData) != "{}" {
		t.Errorf("payload / source_data 应清空，得到 %s / %s", cur.Payload, cur.SourceData)
	}
	if cur.SourceType != "device" {
		t.Errorf("source_type 应为 device，得到 %s", cur.SourceType)
	}
	if cur.TextFormat != "plain" {
		t.Errorf("未显式给格式时应回到 plain，得到 %s", cur.TextFormat)
	}
	// 正文是客户端混淆的字节，服务端只能原样保留
	if string(cur.TextContent) != "obfuscated" {
		t.Error("坍塌不应触碰 text_content")
	}
}

func TestApplyFlattenKeepsExplicitTextFormat(t *testing.T) {
	cur := specialSnippet()
	f := "markdown"
	applyFlatten(cur, &UpdateInput{Flatten: true, TextFormat: &f})
	if cur.TextFormat != "markdown" {
		t.Errorf("请求里显式给出的格式应保留，得到 %s", cur.TextFormat)
	}
}

// 对 normal 重复提交 flatten 不能把它的格式悄悄改回 plain
func TestApplyFlattenIsNoopOnNormal(t *testing.T) {
	cur := &Snippet{Type: "normal", TextFormat: "markdown", Payload: json.RawMessage(`{"x":1}`)}
	applyFlatten(cur, &UpdateInput{Flatten: true})
	if cur.TextFormat != "markdown" || string(cur.Payload) != `{"x":1}` {
		t.Errorf("normal 上的 flatten 应为空操作，得到 %+v", cur)
	}
}

func TestApplyFlattenRequiresFlag(t *testing.T) {
	cur := specialSnippet()
	applyFlatten(cur, &UpdateInput{})
	if cur.Type != "special" {
		t.Error("未带 flatten 的普通编辑不能改变类型")
	}
}

// 坍塌落在 UPDATE 语句上（新增的四列与 version 自增在同一事务），纯 Go 单测碰不到，
// 所以这条打真库。未设变量即跳过；只能指向显式提供的一次性测试库：
//
//	SQUYRRL_TEST_DATABASE_URL='postgres://USER:PASS@HOST:5432/disposable_test_db' go test ./internal/features/snippet/ -run Flatten -v
func TestRepoUpdateFlatten(t *testing.T) {
	url := os.Getenv("SQUYRRL_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("未设 SQUYRRL_TEST_DATABASE_URL，跳过需要真库的测试")
	}
	ctx := context.Background()
	pool, err := db.Connect(ctx, url)
	if err != nil {
		t.Fatalf("连接测试库失败: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := db.Migrate(pool); err != nil {
		t.Fatalf("测试库迁移失败: %v", err)
	}

	var userID uuid.UUID
	email := "flatten-" + uuid.NewString()[:8] + "@example.test"
	if err := pool.QueryRow(ctx, `INSERT INTO users (email) VALUES ($1) RETURNING id`, email).Scan(&userID); err != nil {
		t.Fatalf("建用户失败: %v", err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, userID) })

	repo := NewRepo(pool)
	sub := "telegram_message"
	created, err := repo.Create(ctx, userID, &CreateInput{
		Type:        "special",
		Subtype:     &sub,
		TextContent: []byte("old"),
		Payload:     json.RawMessage(`{"url":"https://t.me/c/1/2"}`),
		SourceType:  "upstream",
		SourceData:  json.RawMessage(`{"provider":"telegram"}`),
	})
	if err != nil {
		t.Fatalf("建碎片失败: %v", err)
	}

	got, err := repo.Update(ctx, userID, created.ID, &UpdateInput{
		Version:     created.Version,
		TextContent: []byte("new"),
		Flatten:     true,
	})
	if err != nil {
		t.Fatalf("坍塌失败: %v", err)
	}
	if got.ID != created.ID || !got.CreatedAt.Equal(created.CreatedAt) {
		t.Error("坍塌应原地进行：id 与 created_at 不变")
	}
	if got.Version != created.Version+1 {
		t.Errorf("version 应 +1，得到 %d", got.Version)
	}
	if got.Type != "normal" || got.Subtype != nil || got.SourceType != "device" {
		t.Errorf("类型列未坍塌：%s/%v/%s", got.Type, got.Subtype, got.SourceType)
	}
	if string(got.TextContent) != "new" {
		t.Error("同一请求里的编辑应一并生效")
	}
}

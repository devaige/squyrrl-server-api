package snippet

import (
	"context"
	"testing"

	"github.com/google/uuid"
)

func TestClampAdminLimit(t *testing.T) {
	cases := []struct{ in, want int }{
		{0, AdminDefaultLimit},
		{-1, AdminDefaultLimit},
		{1, 1},
		{AdminMaxLimit, AdminMaxLimit},
		{AdminMaxLimit + 1, AdminMaxLimit},
	}
	for _, c := range cases {
		if got := clampAdminLimit(c.in); got != c.want {
			t.Errorf("clampAdminLimit(%d) = %d，期望 %d", c.in, got, c.want)
		}
	}
}

// SQUYRRL_TEST_DATABASE_URL='postgres://USER:PASS@HOST:5432/disposable_test_db' go test ./internal/features/snippet/ -run AdminList -v
func TestAdminListPagesTrashAndNames(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	newUser := func() uuid.UUID {
		var id uuid.UUID
		if err := pool.QueryRow(ctx, `INSERT INTO users (email) VALUES ($1) RETURNING id`,
			"admin-list-"+uuid.NewString()+"@example.test").Scan(&id); err != nil {
			t.Fatalf("建用户失败: %v", err)
		}
		return id
	}
	userID, other := newUser(), newUser()

	var pageID, tagID uuid.UUID
	if err := pool.QueryRow(ctx, `INSERT INTO pages (user_id, name) VALUES ($1, '读书') RETURNING id`,
		userID).Scan(&pageID); err != nil {
		t.Fatalf("建页面失败: %v", err)
	}
	if err := pool.QueryRow(ctx, `INSERT INTO tags (user_id, name) VALUES ($1, '稍后读') RETURNING id`,
		userID).Scan(&tagID); err != nil {
		t.Fatalf("建标签失败: %v", err)
	}

	repo := NewRepo(pool)
	create := func(uid uuid.UUID, in *CreateInput) *Snippet {
		s, err := repo.Create(ctx, uid, in)
		if err != nil {
			t.Fatalf("建碎片失败: %v", err)
		}
		return s
	}
	create(userID, &CreateInput{Type: "normal", SourceType: "device", PageID: &pageID, TagIDs: []uuid.UUID{tagID}})
	create(userID, &CreateInput{Type: "normal", SourceType: "device"})
	trashed := create(userID, &CreateInput{Type: "normal", SourceType: "device"})
	if err := repo.SoftDelete(ctx, userID, trashed.ID, trashed.Version); err != nil {
		t.Fatalf("删碎片失败: %v", err)
	}
	// 别人的碎片不能混进来
	create(other, &CreateInput{Type: "normal", SourceType: "device"})

	admin := NewAdminRepo(pool)

	out, err := admin.List(ctx, userID, AdminListInput{})
	if err != nil {
		t.Fatal(err)
	}
	if out.Total != 2 || len(out.Items) != 2 {
		t.Fatalf("默认不含回收站：total=%d items=%d，期望 2/2", out.Total, len(out.Items))
	}
	if out.Pages[pageID] != "读书" || out.Tags[tagID] != "稍后读" {
		t.Fatalf("页面/标签名没带回: pages=%v tags=%v", out.Pages, out.Tags)
	}

	out, err = admin.List(ctx, userID, AdminListInput{IncludeDeleted: true})
	if err != nil {
		t.Fatal(err)
	}
	if out.Total != 3 {
		t.Fatalf("含回收站 total=%d，期望 3", out.Total)
	}

	// total 是全量，items 只是这一页
	out, err = admin.List(ctx, userID, AdminListInput{IncludeDeleted: true, Limit: 2, Offset: 2})
	if err != nil {
		t.Fatal(err)
	}
	if out.Total != 3 || len(out.Items) != 1 {
		t.Fatalf("第二页 total=%d items=%d，期望 3/1", out.Total, len(out.Items))
	}
}

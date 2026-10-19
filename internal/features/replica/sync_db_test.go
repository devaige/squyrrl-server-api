package replica

import (
	"context"
	"crypto/rand"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/squyrrl/api/internal/features/entitlement"
	"github.com/squyrrl/api/internal/features/file"
	"github.com/squyrrl/api/internal/features/page"
	"github.com/squyrrl/api/internal/features/quota"
	"github.com/squyrrl/api/internal/features/snippet"
	"github.com/squyrrl/api/internal/features/tag"
	"github.com/squyrrl/api/internal/infra/db"
)

// 这些测试打真库，只能指向显式提供的一次性测试库：
//
//	SQUYRRL_TEST_DATABASE_URL='postgres://USER:PASS@HOST:5432/disposable_test_db' go test ./internal/features/replica/ -v

type plans map[uuid.UUID]string

func (p plans) ActivePlan(_ context.Context, userID uuid.UUID) (string, error) {
	if plan, ok := p[userID]; ok {
		return plan, nil
	}
	return entitlement.Free, nil
}

type fixture struct {
	t     *testing.T
	ctx   context.Context
	pool  *pgxpool.Pool
	plans plans
	svc   *Service
	snips *snippet.Repo
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
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
	p := plans{}
	q := quota.NewService(pool, p)
	snips := snippet.NewRepo(pool)
	svc := NewService(pool, q, snippet.NewService(snips, q), page.NewService(page.NewRepo(pool), q), tag.NewRepo(pool))
	return &fixture{t: t, ctx: ctx, pool: pool, plans: p, svc: svc, snips: snips}
}

// user 建一个账号，默认 basic 档（会同步）。
func (f *fixture) user(plan string) uuid.UUID {
	f.t.Helper()
	var id uuid.UUID
	if err := f.pool.QueryRow(f.ctx, `INSERT INTO users (email) VALUES ($1) RETURNING id`,
		"sync-"+uuid.NewString()+"@example.test").Scan(&id); err != nil {
		f.t.Fatalf("建用户失败: %v", err)
	}
	f.plans[id] = plan
	f.t.Cleanup(func() { _, _ = f.pool.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, id) })
	return id
}

func (f *fixture) push(userID uuid.UUID, muts ...Mutation) []Result {
	f.t.Helper()
	res, err := f.svc.Push(f.ctx, userID, muts)
	if err != nil {
		f.t.Fatalf("推送失败: %v", err)
	}
	return res
}

func (f *fixture) one(userID uuid.UUID, m Mutation) Result {
	f.t.Helper()
	return f.push(userID, m)[0]
}

func (f *fixture) pull(userID uuid.UUID, since int64) *PullOutput {
	f.t.Helper()
	out, err := f.svc.Pull(f.ctx, userID, since, 0)
	if err != nil {
		f.t.Fatalf("拉取失败: %v", err)
	}
	return out
}

func ptr[T any](v T) *T { return &v }

func content(title, text string) *snippet.PutInput {
	return &snippet.PutInput{Content: snippet.Content{
		Type: "normal", Title: ptr(title), TextContent: []byte(text), SourceType: "device",
	}}
}

func putSnippet(id uuid.UUID, base int64, in *snippet.PutInput) Mutation {
	return Mutation{Entity: EntitySnippet, Op: OpPut, ID: id, BaseVersion: base, Snippet: in}
}

func TestPullFollowsSeqAndSkipsDeletesOnFirstSync(t *testing.T) {
	f := newFixture(t)
	u := f.user(entitlement.Basic)
	pageID, tagID, keep, drop := uuid.New(), uuid.New(), uuid.New(), uuid.New()

	created := time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC) // 时钟拨快的设备：原样存，只用来显示
	in := content("保留", "正文")
	in.CreatedAt, in.UpdatedAt = &created, &created
	in.PageID, in.TagIDs = &pageID, []uuid.UUID{tagID}
	res := f.push(u,
		Mutation{Entity: EntityPage, Op: OpPut, ID: pageID, Page: &page.PutInput{Name: "读书"}},
		Mutation{Entity: EntityTag, Op: OpPut, ID: tagID, Tag: &tag.PutInput{Name: "稍后读"}},
		putSnippet(keep, 0, in),
		putSnippet(drop, 0, content("删掉", "x")),
		Mutation{Entity: EntitySnippet, Op: OpDelete, ID: drop, BaseVersion: 1},
	)
	for i, r := range res {
		if r.Status != StatusOK {
			t.Fatalf("第 %d 条应成功，得到 %+v", i, r)
		}
	}
	got := res[2].Snippet
	if !got.CreatedAt.Equal(created) || !got.UpdatedAt.Equal(created) {
		t.Errorf("客户端时间应原样保存，得到 %v / %v", got.CreatedAt, got.UpdatedAt)
	}
	if got.PageID == nil || *got.PageID != pageID || !slices.Equal(got.TagIDs, []uuid.UUID{tagID}) {
		t.Errorf("新建时的归类没挂上：%v %v", got.PageID, got.TagIDs)
	}

	first := f.pull(u, 0)
	if len(first.Pages) != 1 || len(first.Tags) != 1 || len(first.Snippets) != 1 {
		t.Fatalf("首次同步应拿到 1 页 1 标签 1 碎片，得到 %d/%d/%d",
			len(first.Pages), len(first.Tags), len(first.Snippets))
	}
	if len(first.Deleted.Snippets) != 0 {
		t.Error("首次同步不该下发删除标记")
	}
	if first.HasMore || first.Cursor == 0 {
		t.Errorf("游标 %d has_more %v", first.Cursor, first.HasMore)
	}
	if again := f.pull(u, first.Cursor); len(again.Snippets)+len(again.Pages)+len(again.Tags) != 0 ||
		again.Cursor != first.Cursor {
		t.Errorf("没有新改动时应为空且游标不动：%+v", again)
	}

	// 从更早的游标拉，删除标记要出现
	if old := f.pull(u, 1); !slices.Contains(old.Deleted.Snippets, drop) {
		t.Errorf("增量里应带上删除标记，得到 %v", old.Deleted.Snippets)
	}
}

func TestPullPagesThroughLimit(t *testing.T) {
	f := newFixture(t)
	u := f.user(entitlement.Basic)
	var muts []Mutation
	for i := 0; i < 7; i++ {
		muts = append(muts, putSnippet(uuid.New(), 0, content("n", "x")))
	}
	f.push(u, muts...)

	seen := map[uuid.UUID]bool{}
	var cursor int64
	for range 10 {
		out, err := f.svc.Pull(f.ctx, u, cursor, 3)
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range out.Snippets {
			seen[s.ID] = true
		}
		cursor = out.Cursor
		if !out.HasMore {
			break
		}
	}
	if len(seen) != 7 {
		t.Errorf("分页拉完应拿到 7 条，得到 %d", len(seen))
	}
}

func TestPutIsIdempotentAndRetriedCreateAppliesLatest(t *testing.T) {
	f := newFixture(t)
	u := f.user(entitlement.Basic)
	id := uuid.New()

	a := f.one(u, putSnippet(id, 0, content("t", "v1")))
	// 响应丢了，原样重发：内容相同直接成功，不造冲突、不涨版本
	b := f.one(u, putSnippet(id, 0, content("t", "v1")))
	if b.Status != StatusOK || b.Snippet.Version != a.Snippet.Version {
		t.Fatalf("重发相同内容应原样成功，得到 %+v", b)
	}
	// 响应丢了期间本机又改了：发件箱里还是那条新建，带着最新内容
	c := f.one(u, putSnippet(id, 0, content("t", "v2")))
	if c.Status != StatusOK || string(c.Snippet.TextContent) != "v2" || c.Snippet.Version != 2 {
		t.Fatalf("已存在的新建应按 base=1 应用最新内容，得到 %+v", c)
	}
}

func TestContentConflictArchivesLoserWithElements(t *testing.T) {
	f := newFixture(t)
	u := f.user(entitlement.Basic)
	id := uuid.New()
	f.one(u, putSnippet(id, 0, content("t", "原文")))

	// 设备 A 先改
	if r := f.one(u, putSnippet(id, 1, content("t", "A 改的"))); r.Status != StatusOK {
		t.Fatalf("A 应成功：%+v", r)
	}
	// 设备 B 基于同一个旧版本改，并加了附件
	fileID := newFile(t, f)
	in := content("t", "B 改的")
	in.Elements = []snippet.ElementInput{{FileID: fileID}}
	r := f.one(u, putSnippet(id, 1, in))
	if r.Status != StatusConflict || r.LoserID == nil {
		t.Fatalf("B 应冲突并留副本：%+v", r)
	}
	if string(r.Snippet.TextContent) != "A 改的" {
		t.Errorf("服务端应保留先到的 A，得到 %q", r.Snippet.TextContent)
	}
	loser, err := f.snips.Get(f.ctx, u, *r.LoserID)
	if err != nil {
		t.Fatal(err)
	}
	if string(loser.TextContent) != "B 改的" || len(loser.Elements) != 1 || loser.Elements[0].FileID != fileID {
		t.Errorf("冲突副本应带着 B 的正文与附件：%q %+v", loser.TextContent, loser.Elements)
	}
}

func TestEditSurvivesDeleteEitherWay(t *testing.T) {
	f := newFixture(t)
	u := f.user(entitlement.Basic)

	// 删除先到：修改进冲突收件箱
	id := uuid.New()
	f.one(u, putSnippet(id, 0, content("t", "原文")))
	if r := f.one(u, Mutation{Entity: EntitySnippet, Op: OpDelete, ID: id, BaseVersion: 1}); !r.Deleted {
		t.Fatalf("删除应成功：%+v", r)
	}
	r := f.one(u, putSnippet(id, 1, content("t", "离线时改的")))
	if r.Status != StatusConflict || !r.Deleted || r.LoserID == nil {
		t.Fatalf("删除先到时修改应进冲突收件箱：%+v", r)
	}

	// 修改先到：基于旧版本的删除被拒，碎片留着
	id2 := uuid.New()
	f.one(u, putSnippet(id2, 0, content("t", "原文")))
	f.one(u, putSnippet(id2, 1, content("t", "新内容")))
	r = f.one(u, Mutation{Entity: EntitySnippet, Op: OpDelete, ID: id2, BaseVersion: 1})
	if r.Status != StatusConflict || r.Deleted || r.Snippet == nil || string(r.Snippet.TextContent) != "新内容" {
		t.Fatalf("修改先到时删除应被拒：%+v", r)
	}
}

func TestOrganisationDoesNotBumpVersion(t *testing.T) {
	f := newFixture(t)
	u := f.user(entitlement.Basic)
	id, p1, t1, t2 := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	f.push(u,
		Mutation{Entity: EntityPage, Op: OpPut, ID: p1, Page: &page.PutInput{Name: "同名"}},
		// 同名页面、同名标签都允许：身份只认 UUID
		Mutation{Entity: EntityPage, Op: OpPut, ID: uuid.New(), Page: &page.PutInput{Name: "同名"}},
		Mutation{Entity: EntityTag, Op: OpPut, ID: t1, Tag: &tag.PutInput{Name: "同名"}},
		Mutation{Entity: EntityTag, Op: OpPut, ID: t2, Tag: &tag.PutInput{Name: "同名"}},
		putSnippet(id, 0, content("t", "x")),
	)

	moved := f.one(u, Mutation{Entity: EntitySnippet, Op: OpMove, ID: id, PageID: &p1})
	// 两台设备各加一个标签，两个都在
	f.one(u, Mutation{Entity: EntitySnippet, Op: OpTags, ID: id, Add: []uuid.UUID{t1}})
	tagged := f.one(u, Mutation{Entity: EntitySnippet, Op: OpTags, ID: id, Add: []uuid.UUID{t2}})
	if moved.Snippet.PageID == nil || *moved.Snippet.PageID != p1 {
		t.Fatalf("移动没生效：%+v", moved.Snippet.PageID)
	}
	if len(tagged.Snippet.TagIDs) != 2 || tagged.Snippet.Version != 1 {
		t.Fatalf("标签应合并为两个、版本不变，得到 %v v%d", tagged.Snippet.TagIDs, tagged.Snippet.Version)
	}
	// 归类变化之后，基于 v1 的正文编辑照常成功
	if r := f.one(u, putSnippet(id, 1, content("t", "改正文"))); r.Status != StatusOK {
		t.Fatalf("归类变化不该让正文编辑冲突：%+v", r)
	}

	// 移进一个不存在的页面：忽略，返回当前状态
	r := f.one(u, Mutation{Entity: EntitySnippet, Op: OpMove, ID: id, PageID: ptr(uuid.New())})
	if r.Status != StatusOK || *r.Snippet.PageID != p1 {
		t.Errorf("目标页面不存在时应保持原样：%+v", r)
	}
	// 新建还没推上来就移动：稍后重试
	if r := f.one(u, Mutation{Entity: EntitySnippet, Op: OpMove, ID: uuid.New(), PageID: &p1}); r.Status != StatusRetry {
		t.Errorf("对不存在的碎片移动应要求重试：%+v", r)
	}
}

func TestDeletingTagDetachesAndResyncsSnippets(t *testing.T) {
	f := newFixture(t)
	u := f.user(entitlement.Basic)
	id, tg := uuid.New(), uuid.New()
	in := content("t", "x")
	in.TagIDs = []uuid.UUID{tg}
	f.push(u,
		Mutation{Entity: EntityTag, Op: OpPut, ID: tg, Tag: &tag.PutInput{Name: "将删"}},
		putSnippet(id, 0, in),
	)
	cursor := f.pull(u, 0).Cursor

	if r := f.one(u, Mutation{Entity: EntityTag, Op: OpDelete, ID: tg}); !r.Deleted {
		t.Fatalf("删标签失败：%+v", r)
	}
	out := f.pull(u, cursor)
	if !slices.Contains(out.Deleted.Tags, tg) {
		t.Error("标签删除应随增量下发")
	}
	if len(out.Snippets) != 1 || len(out.Snippets[0].TagIDs) != 0 {
		t.Errorf("被摘掉标签的碎片应带着新的标签集合重新下发：%+v", out.Snippets)
	}
	// 已删的标签不会被迟到的改名复活
	if r := f.one(u, Mutation{Entity: EntityTag, Op: OpPut, ID: tg, Tag: &tag.PutInput{Name: "改名"}}); !r.Deleted {
		t.Errorf("迟到的改名不该复活已删标签：%+v", r)
	}
}

func TestThumbnailBackfillResyncsSnippet(t *testing.T) {
	f := newFixture(t)
	u := f.user(entitlement.Basic)
	fileID := newFile(t, f)
	in := content("t", "x")
	in.Elements = []snippet.ElementInput{{FileID: fileID}}
	f.one(u, putSnippet(uuid.New(), 0, in))
	cursor := f.pull(u, 0).Cursor

	if _, err := file.NewRepo(f.pool).SetThumbnail(f.ctx, fileID, "thumb/x.jpg", file.Dims{Width: 10, Height: 10}); err != nil {
		t.Fatal(err)
	}
	out := f.pull(u, cursor)
	if len(out.Snippets) != 1 || !out.Snippets[0].Elements[0].HasThumbnail {
		t.Errorf("缩略图挂上后碎片应重新下发：%+v", out.Snippets)
	}
}

func TestTrashPurgeLeavesTombstone(t *testing.T) {
	f := newFixture(t)
	u := f.user(entitlement.Basic)
	id := uuid.New()
	f.one(u, putSnippet(id, 0, content("t", "x")))
	staleCursor := f.pull(u, 0).Cursor // 这台设备此后离线很久
	f.one(u, Mutation{Entity: EntitySnippet, Op: OpDelete, ID: id, BaseVersion: 1})
	if _, err := f.pool.Exec(f.ctx,
		`UPDATE snippets SET deleted_at = now() - interval '40 days' WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
	if _, err := snippet.NewTrashSweeper(f.pool, f.plans, time.Hour).Sweep(f.ctx); err != nil {
		t.Fatal(err)
	}

	out := f.pull(u, staleCursor)
	if !slices.Contains(out.Deleted.Snippets, id) {
		t.Errorf("硬删之后，离线设备仍应从墓碑得知删除：%+v", out.Deleted)
	}
	// 离线期间改过它：删除先到，修改进冲突收件箱
	if r := f.one(u, putSnippet(id, 1, content("t", "离线修改"))); r.Status != StatusConflict || r.LoserID == nil {
		t.Errorf("对已清理的碎片的修改应进冲突收件箱：%+v", r)
	}
}

func TestRestrictionPurgeKeepsLocalCopiesViaReconcile(t *testing.T) {
	f := newFixture(t)
	u := f.user(entitlement.Basic)
	gone, kept := uuid.New(), uuid.New()
	f.push(u, putSnippet(gone, 0, content("t", "超额")), putSnippet(kept, 0, content("t", "额度内")))
	before := f.pull(u, 0)

	if _, err := f.pool.Exec(f.ctx,
		`UPDATE snippets SET restricted_at = now() - interval '61 days' WHERE id = $1`, gone); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := snippet.NewRestrictionSweeper(f.pool, f.plans, time.Hour).Sweep(f.ctx); err != nil {
		t.Fatal(err)
	}

	after := f.pull(u, before.Cursor)
	if slices.Contains(after.Deleted.Snippets, gone) {
		t.Error("降级清理只删云端副本，不能下发删除标记")
	}
	if after.ReconcileGen != before.ReconcileGen+1 {
		t.Errorf("reconcile_gen 应 +1：%d → %d", before.ReconcileGen, after.ReconcileGen)
	}
	m, err := f.svc.Manifest(f.ctx, u)
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(m.SnippetIDs, gone) || !slices.Contains(m.SnippetIDs, kept) {
		t.Errorf("清单应只剩额度内的那条：%v", m.SnippetIDs)
	}
	// 升级回来后本机那份重新上传：同一个 id 重建
	if r := f.one(u, putSnippet(gone, 1, content("t", "超额"))); r.Status != StatusOK || r.Snippet.ID != gone {
		t.Errorf("被清理的碎片应能按原 id 重新上传：%+v", r)
	}
}

func TestFreePlanCannotPush(t *testing.T) {
	f := newFixture(t)
	u := f.user(entitlement.Free)
	_, err := f.svc.Push(f.ctx, u, []Mutation{putSnippet(uuid.New(), 0, content("t", "x"))})
	if qe, ok := quota.IsQuotaError(err); !ok || qe.Reason != quota.ReasonSyncRequired {
		t.Errorf("免费档推送应整批 402 sync_required，得到 %v", err)
	}
}

func TestQuotaRejectionIsPerMutation(t *testing.T) {
	f := newFixture(t)
	u := f.user(entitlement.Basic)
	var muts []Mutation
	for i := 0; i < entitlement.MustOf(entitlement.Basic).Pages+1; i++ {
		muts = append(muts, Mutation{Entity: EntityPage, Op: OpPut, ID: uuid.New(), Page: &page.PutInput{Name: "p"}})
	}
	muts = append(muts, putSnippet(uuid.New(), 0, content("t", "x")))
	res := f.push(u, muts...)
	last := res[len(res)-2]
	if last.Status != StatusRejected || last.Code != CodeQuota || last.Quota == nil {
		t.Errorf("超出页面额度的那条应被拒并带额度信息：%+v", last)
	}
	if res[len(res)-1].Status != StatusOK {
		t.Errorf("同一批里其它改动不受影响：%+v", res[len(res)-1])
	}
}

// 同一账号的写入按号的顺序提交：后领号的事务要等先领号的提交。
// 否则拉取可能先看见 11、事后才冒出 10，而游标已经越过了 10。
func TestSeqCommitsInOrderPerUser(t *testing.T) {
	f := newFixture(t)
	u := f.user(entitlement.Basic)

	txA, err := f.pool.Begin(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	var seqA int64
	if err := txA.QueryRow(f.ctx,
		`INSERT INTO pages (user_id, name) VALUES ($1, 'A') RETURNING sync_seq`, u).Scan(&seqA); err != nil {
		t.Fatal(err)
	}

	done := make(chan int64, 1)
	go func() {
		var seqB int64
		_ = f.pool.QueryRow(context.Background(),
			`INSERT INTO pages (user_id, name) VALUES ($1, 'B') RETURNING sync_seq`, u).Scan(&seqB)
		done <- seqB
	}()
	select {
	case <-done:
		t.Fatal("B 不该在 A 提交前领到号")
	case <-time.After(300 * time.Millisecond):
	}
	if err := txA.Commit(f.ctx); err != nil {
		t.Fatal(err)
	}
	if seqB := <-done; seqB <= seqA {
		t.Errorf("B 的号 %d 应大于 A 的号 %d", seqB, seqA)
	}
}

func TestRESTPatchOnlyChecksVersionForContent(t *testing.T) {
	f := newFixture(t)
	u := f.user(entitlement.Basic)
	id, p := uuid.New(), uuid.New()
	f.push(u,
		Mutation{Entity: EntityPage, Op: OpPut, ID: p, Page: &page.PutInput{Name: "p"}},
		putSnippet(id, 0, content("t", "x")))

	// 网页端带着过期的版本只移动页面：不核对、不涨版本、不改 updated_at
	before, _ := f.snips.Get(f.ctx, u, id)
	got, err := f.snips.Update(f.ctx, u, id, &snippet.UpdateInput{Version: 99, PageID: &p})
	if err != nil {
		t.Fatalf("只移动页面不该核对版本：%v", err)
	}
	if got.Version != before.Version || !got.UpdatedAt.Equal(before.UpdatedAt) {
		t.Errorf("移动不该改版本与 updated_at：v%d→v%d", before.Version, got.Version)
	}
	if _, err := f.snips.Update(f.ctx, u, id, &snippet.UpdateInput{Version: 99, Title: ptr("x")}); err == nil {
		t.Error("改正文时版本不对应当冲突")
	}
}

func newFile(t *testing.T, f *fixture) uuid.UUID {
	t.Helper()
	rnd := func() []byte {
		b := make([]byte, 32)
		_, _ = rand.Read(b)
		return b
	}
	cipher := rnd()
	fl, err := file.NewRepo(f.pool).Insert(f.ctx, rnd(), cipher, 10, "image/png", "squyrrl",
		file.StorageKeyFor(cipher), file.Dims{})
	if err != nil {
		t.Fatalf("建文件失败: %v", err)
	}
	return fl.ID
}

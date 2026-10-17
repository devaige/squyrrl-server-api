package extapi

import (
	"encoding/json"
	"slices"
	"testing"
)

func archived(data string) json.RawMessage {
	return json.RawMessage(`{"status":200,"body":{"code":200,"request_id":"x","data":` + data + `}}`)
}

// 缺失只算「所有示例都有」的字段：图片推文没有 media.video 不算缺；连 author 都没有才是改版。
// 外壳（request_id 之类）不参与比较。
func TestStructureDiff(t *testing.T) {
	photo := archived(`{"id":"1","text":"a","author":{"name":"n"},"media":{"photo":[{"url":"u"}]}}`)
	video := archived(`{"id":"2","text":"b","author":{"name":"n"},"media":{"video":[{"url":"u","variants":[{"bitrate":1}]}]}}`)
	baselines := []json.RawMessage{photo, video}

	same := diffAgainst(archived(`{"id":"3","text":"c","author":{"name":"m"},"media":{"photo":[{"url":"v"},{"url":"w"}]}}`), baselines, nil, true)
	if same.MissingCount != 0 || same.AddedCount != 0 || same.Baseline != 2 {
		t.Errorf("同结构不该有差异: %+v", same)
	}

	changed := diffAgainst(archived(`{"id":"4","content":"c","user":{"name":"m"},"media":{}}`), baselines, nil, true)
	if !slices.Equal(changed.Missing, []string{"author", "text"}) {
		t.Errorf("缺失应只列最浅一层的共有字段，得到 %v", changed.Missing)
	}
	if !slices.Equal(changed.Added, []string{"content", "user"}) {
		t.Errorf("新增应只列最浅一层，得到 %v", changed.Added)
	}
	// 计数与列表同口径：user.name 跟着 user 走，不另算一处
	if changed.MissingCount != 2 || changed.AddedCount != 2 {
		t.Errorf("计数应按分支，得到 缺失 %d / 新增 %d", changed.MissingCount, changed.AddedCount)
	}
	if d := diffAgainst(archived(`{"id":"4","content":"c","user":{"name":"m"},"media":{}}`), baselines, nil, false); d.AddedCount != 2 || d.Added != nil {
		t.Errorf("不要列表时计数照旧、不带列表: %+v", d)
	}

	if d := diffAgainst(photo, nil, nil, true); d.Baseline != 0 || d.MissingCount != 0 {
		t.Errorf("没有示例时无从比较: %+v", d)
	}

	// 只看代码读取的字段：示例里有、这一份没有的 extra 不读，不算缺失
	withExtra := archived(`{"id":"1","text":"a","author":{"name":"n","live":{"room":1}}}`)
	noExtra := archived(`{"id":"2","text":"b","author":{"name":"m"}}`)
	watched := []string{"id", "text", "author", "author.name"}
	if d := diffAgainst(noExtra, []json.RawMessage{withExtra}, watched, true); d.MissingCount != 0 {
		t.Errorf("代码不读的字段缺了不该报: %+v", d)
	}
	if d := diffAgainst(archived(`{"id":"3","author":{"name":"m"}}`), []json.RawMessage{withExtra}, watched, true); !slices.Equal(d.Missing, []string{"text"}) {
		t.Errorf("代码读的字段缺了要报: %+v", d)
	}
}

// 只比上游正常应答的线上采样：失败应答是一段错误说明，比出来的「缺失」不是改版。
func TestDiffableOnlyLiveSuccess(t *testing.T) {
	for _, c := range []struct {
		kind   string
		status int
		want   bool
	}{{"live", 200, true}, {"live", 400, false}, {"live", 0, false}, {"sample", 200, false}} {
		if got := diffable(Sample{Kind: c.kind, HTTPStatus: c.status}); got != c.want {
			t.Errorf("%s/%d → %v，期望 %v", c.kind, c.status, got, c.want)
		}
	}
}

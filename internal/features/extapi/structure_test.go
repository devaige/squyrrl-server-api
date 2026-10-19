package extapi

import (
	"encoding/json"
	"slices"
	"testing"
)

func archived(data string) json.RawMessage {
	return json.RawMessage(`{"status":200,"body":{"code":200,"request_id":"x","data":` + data + `}}`)
}

func bases(examples ...Example) []baseline { return baselinesOf(examples) }

// 缺失按最接近的示例算：图片推文没有 video 不算缺；连 author 都没有才是改版。
// 外壳（request_id 之类）不参与比较。
func TestStructureDiff(t *testing.T) {
	photo := Example{Note: "图片推文", Data: json.RawMessage(`{"id":"1","text":"a","author":{"name":"n"},"photos":[{"url":"u"}]}`)}
	video := Example{Note: "视频推文", Data: json.RawMessage(`{"id":"2","text":"b","author":{"name":"n"},"video":{"url":"u","variants":[{"bitrate":1}]}}`)}
	examples := bases(photo, video)

	same := diffAgainst(archived(`{"id":"3","text":"c","author":{"name":"m"},"photos":[{"url":"v"},{"url":"w"}]}`), examples, nil, true)
	if same.MissingCount != 0 || same.AddedCount != 0 || same.Baseline != 2 || same.Closest != "图片推文" {
		t.Errorf("同结构不该有差异: %+v", same)
	}

	changed := diffAgainst(archived(`{"id":"4","content":"c","user":{"name":"m"},"photos":[]}`), examples, nil, true)
	if changed.Closest != "图片推文" || !slices.Equal(changed.Missing, []string{"author", "photos[].url", "text"}) {
		t.Errorf("缺失应相对最接近的示例、只列最浅一层，得到 %+v", changed)
	}
	if !slices.Equal(changed.Added, []string{"content", "user"}) {
		t.Errorf("新增应只列最浅一层，得到 %v", changed.Added)
	}
	// 计数与列表同口径：user.name 跟着 user 走，不另算一处
	if changed.MissingCount != 3 || changed.AddedCount != 2 {
		t.Errorf("计数应按分支，得到 缺失 %d / 新增 %d", changed.MissingCount, changed.AddedCount)
	}
	if d := diffAgainst(archived(`{"id":"4","content":"c","user":{"name":"m"},"photos":[]}`), examples, nil, false); d.AddedCount != 2 || d.Added != nil {
		t.Errorf("不要列表时计数照旧、不带列表: %+v", d)
	}

	if d := diffAgainst(archived(`{"id":"1"}`), nil, nil, true); d.Baseline != 0 || d.MissingCount != 0 {
		t.Errorf("没有示例时无从比较: %+v", d)
	}
	if d := diffAgainst(archived(`{"id":"1"}`), bases(Example{Data: json.RawMessage(`{`)}), nil, true); d.Baseline != 0 {
		t.Errorf("解析不了的示例不参与比对: %+v", d)
	}

	// 只看代码读取的字段：示例里有、这一份没有的 extra 不读，不算缺失
	withExtra := bases(Example{Data: json.RawMessage(`{"id":"1","text":"a","author":{"name":"n","live":{"room":1}}}`)})
	watched := []string{"id", "text", "author", "author.name"}
	if d := diffAgainst(archived(`{"id":"2","text":"b","author":{"name":"m"}}`), withExtra, watched, true); d.MissingCount != 0 {
		t.Errorf("代码不读的字段缺了不该报: %+v", d)
	}
	if d := diffAgainst(archived(`{"id":"3","author":{"name":"m"}}`), withExtra, watched, true); !slices.Equal(d.Missing, []string{"text"}) {
		t.Errorf("代码读的字段缺了要报: %+v", d)
	}
}

// 多收录一种形态不会放松对别的形态的检查：视频推文丢了 variants，按「所有示例共有的字段」
// 比是看不出来的（图片推文本来就没有 video），和最接近的视频推文比才报得出来。
func TestStructureDiffClosestExample(t *testing.T) {
	photo := Example{Note: "图片推文", Data: json.RawMessage(`{"id":"1","author":{"name":"n"},"photos":[{"url":"u"}]}`)}
	video := Example{Note: "视频推文", Data: json.RawMessage(`{"id":"2","author":{"name":"n"},"video":{"url":"u","variants":[{"bitrate":1}]}}`)}

	d := diffAgainst(archived(`{"id":"3","author":{"name":"m"},"video":{"url":"v"}}`), bases(photo, video), nil, true)
	if d.Closest != "视频推文" || !slices.Equal(d.Missing, []string{"video.variants"}) {
		t.Errorf("应相对视频推文报缺 video.variants，得到 %+v", d)
	}

	// 缺得一样多时，取这一份比它多出字段最少的那份：带着 video 的更像视频推文
	d = diffAgainst(archived(`{"id":"3","author":{"name":"m"},"video":{"url":"v","variants":[]}}`), bases(photo, video), nil, true)
	if d.Closest != "视频推文" {
		t.Errorf("最接近的应是视频推文，得到 %+v", d)
	}
}

// 示例两两比对：收录进来的形态不再在线上采样里报差异，示例之间差在哪要看得到。
// 比全部字段，解析不了的示例不参与，也不出现在别人的对比里。
func TestCompareExamples(t *testing.T) {
	examples := []Example{
		{Note: "图片推文", Data: json.RawMessage(`{"id":"1","author":{"name":"n"},"photos":[{"url":"u"}]}`)},
		{Note: "坏的", Data: json.RawMessage(`{`)},
		{Note: "视频推文", Data: json.RawMessage(`{"id":"2","author":{"name":"n","live":{"room":1}},"video":{"url":"u"}}`)},
	}
	compareExamples(examples, baselinesOf(examples))

	photo, broken, video := examples[0].Versus, examples[1].Versus, examples[2].Versus
	if len(photo) != 1 || photo[0].Other != "视频推文" ||
		!slices.Equal(photo[0].Added, []string{"photos"}) || !slices.Equal(photo[0].Missing, []string{"author.live", "video"}) ||
		photo[0].AddedCount != 1 || photo[0].MissingCount != 2 {
		t.Errorf("图片推文相对视频推文: %+v", photo)
	}
	if len(video) != 1 || video[0].Other != "图片推文" ||
		!slices.Equal(video[0].Added, photo[0].Missing) || !slices.Equal(video[0].Missing, photo[0].Added) {
		t.Errorf("反过来比应正好对调: %+v", video)
	}
	if broken != nil {
		t.Errorf("解析不了的示例不参与比对: %+v", broken)
	}
	one := []Example{{Note: "图片推文", Data: examples[0].Data}}
	if compareExamples(one, baselinesOf(one)); one[0].Versus != nil {
		t.Errorf("只有一份示例时无从比较: %+v", one[0].Versus)
	}
}

// 只比上游正常应答的线上采样：失败应答是一段错误说明，比出来的「缺失」不是改版。
func TestDiffableOnlySuccess(t *testing.T) {
	for _, c := range []struct {
		status int
		want   bool
	}{{200, true}, {204, true}, {400, false}, {0, false}} {
		if got := diffable(Sample{HTTPStatus: c.status}); got != c.want {
			t.Errorf("%d → %v，期望 %v", c.status, got, c.want)
		}
	}
}

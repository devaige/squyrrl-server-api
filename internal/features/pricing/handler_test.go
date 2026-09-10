package pricing

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

// GET /pricing 必须无鉴权可达：购买页与向导在登录前就要能算价。
func TestPricingEndpointIsPublicAndComplete(t *testing.T) {
	gin.SetMode(gin.TestMode)
	e := gin.New()
	NewHandler().RegisterPublic(e.Group("/"))

	w := httptest.NewRecorder()
	e.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/pricing", nil))

	if w.Code != http.StatusOK {
		t.Fatalf("状态码 %d，期望 200", w.Code)
	}
	if cc := w.Header().Get("Cache-Control"); cc != "public, max-age=300" {
		t.Errorf("Cache-Control = %q", cc)
	}

	var got Catalog
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("响应不是合法 JSON：%v", err)
	}
	if got.CreditsPerUSD != CreditsPerUSD {
		t.Errorf("汇率 %d，期望 %d", got.CreditsPerUSD, CreditsPerUSD)
	}
	if len(got.Plans) != 5 {
		t.Errorf("档位 %d 个，期望 5", len(got.Plans))
	}
	if got.Plans[0].Key != "free" || got.Plans[4].Key != "maximum" {
		t.Errorf("档位顺序不对：%s … %s", got.Plans[0].Key, got.Plans[4].Key)
	}
	// 三块商品缺一不可 —— 少一块客户端就无法解释 402
	if len(got.Storage) == 0 || len(got.CreditPacks) == 0 || len(got.MaxFileSize) == 0 {
		t.Error("价目表缺少存储 / 代币 / 单文件上限之一")
	}
	// ADR-075 的硬约束：任何档位都不自带存储或代币
	for _, p := range got.Plans {
		b, _ := json.Marshal(p)
		var m map[string]any
		json.Unmarshal(b, &m)
		for _, k := range []string{"credits", "storage", "storage_gb", "monthly_credits"} {
			if _, bad := m[k]; bad {
				t.Errorf("档位 %s 的响应里出现了 %q —— 基础订阅不该附带代币或存储", p.Key, k)
			}
		}
	}
}

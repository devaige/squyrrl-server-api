package extapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TikHub 的数据在外壳的 data 里；HTTP 200 即计费，哪怕 data 是空的。
func TestTikHubUnwrap(t *testing.T) {
	cases := []struct {
		name       string
		status     int
		body       string
		wantKind   string
		wantBilled bool
		wantData   string
	}{
		{"成功", 200, `{"code":200,"message":"ok","data":{"title":"x"}}`, "", true, `{"title":"x"}`},
		{"data 为空仍计费", 200, `{"code":200,"data":null}`, KindEmpty, true, ""},
		{"外壳 code 异常", 200, `{"code":500,"message":"boom","data":{"a":1}}`, KindBadResponse, true, ""},
		{"不是 JSON", 200, `<html>`, KindBadResponse, true, ""},
		{"余额不足不计费", 402, `{"detail":"Insufficient balance"}`, KindHTTPStatus, false, ""},
		{"限流不计费", 429, `{"detail":{"message":"Too many requests"}}`, KindHTTPStatus, false, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			data, billed, err := tikhub{}.unwrap(c.status, []byte(c.body))
			if billed != c.wantBilled {
				t.Errorf("billed=%v，期望 %v", billed, c.wantBilled)
			}
			if c.wantKind == "" {
				if err != nil {
					t.Fatalf("不应失败: %v", err)
				}
				if string(data) != c.wantData {
					t.Errorf("data=%s，期望 %s", data, c.wantData)
				}
				return
			}
			if err == nil || err.Kind != c.wantKind {
				t.Fatalf("期望失败类别 %s，得到 %v", c.wantKind, err)
			}
		})
	}
}

// RapidAPI 不加外壳：响应体原样就是数据。
func TestRapidAPIUnwrap(t *testing.T) {
	data, billed, err := rapidAPI{}.unwrap(200, []byte(`{"title":"x"}`))
	if err != nil || !billed || string(data) != `{"title":"x"}` {
		t.Fatalf("成功响应应原样返回且计费：data=%s billed=%v err=%v", data, billed, err)
	}
	_, billed, err = (rapidAPI{}).unwrap(403, []byte(`{"message":"You are not subscribed"}`))
	if err == nil || err.Kind != KindHTTPStatus || billed || err.Msg != "You are not subscribed" {
		t.Fatalf("403 应为不计费的 http_status，并带上上游原话：billed=%v err=%v", billed, err)
	}
}

// fakeUpstream 记下收到的请求头，按路径返回预设响应。
func fakeUpstream(t *testing.T, handler http.HandlerFunc) (*httptest.Server, *http.Header) {
	t.Helper()
	var got http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		handler(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv, &got
}

// 单价为 0 时 Call 不碰数据库，可以不带 repo 测完整的请求路径。
func freeEndpoint(vendor string, cfg string) Endpoint {
	return Endpoint{Slug: "t", Vendor: vendor, Config: json.RawMessage(cfg)}
}

func TestCallTikHubSendsBearerAndQuery(t *testing.T) {
	srv, hdr := fakeUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("share_url") != "https://v.douyin.com/abc/" {
			http.Error(w, `{"detail":"bad query"}`, 400)
			return
		}
		_, _ = w.Write([]byte(`{"code":200,"data":{"ok":true}}`))
	})
	svc := NewService(nil, Credentials{TikHub: "secret-token"}, nil)
	res := svc.Call(context.Background(), freeEndpoint(VendorTikHub, `{}`), Request{
		URL:   srv.URL + "/api/v1/douyin/app/v3/fetch_one_video_by_share_url",
		Query: map[string]string{"share_url": "https://v.douyin.com/abc/"},
	}, "abc")
	if res.Err != nil {
		t.Fatalf("调用失败: %v", res.Err)
	}
	if got := hdr.Get("Authorization"); got != "Bearer secret-token" {
		t.Errorf("Authorization=%q", got)
	}
	if string(res.Data) != `{"ok":true}` || !res.Billed || res.HTTPStatus != 200 {
		t.Errorf("结果不对: data=%s billed=%v status=%d", res.Data, res.Billed, res.HTTPStatus)
	}
}

func TestCallRapidAPISendsKeyAndHost(t *testing.T) {
	srv, hdr := fakeUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"title":"x"}`))
	})
	svc := NewService(nil, Credentials{RapidAPI: "rk"}, nil)
	res := svc.Call(context.Background(), freeEndpoint(VendorRapidAPI, `{}`), Request{URL: srv.URL + "/v2/video"}, "id")
	if res.Err != nil {
		t.Fatalf("调用失败: %v", res.Err)
	}
	if hdr.Get("x-rapidapi-key") != "rk" || hdr.Get("x-rapidapi-host") != strings.TrimPrefix(srv.URL, "http://") {
		t.Errorf("RapidAPI 鉴权头不对: key=%q host=%q", hdr.Get("x-rapidapi-key"), hdr.Get("x-rapidapi-host"))
	}
}

// 没配密钥：不发请求、不计费，链路继续试下一个。
func TestCallWithoutCredentialDoesNotHitUpstream(t *testing.T) {
	hit := false
	srv, _ := fakeUpstream(t, func(w http.ResponseWriter, r *http.Request) { hit = true })
	svc := NewService(nil, Credentials{}, nil)
	res := svc.Call(context.Background(), freeEndpoint(VendorTikHub, `{}`), Request{URL: srv.URL}, "id")
	if res.Err == nil || res.Err.Kind != KindNoCredential || res.Billed || hit {
		t.Fatalf("应在发请求前失败：err=%v billed=%v hit=%v", res.Err, res.Billed, hit)
	}
}

func TestCallTimeout(t *testing.T) {
	srv, _ := fakeUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(2 * time.Second):
		}
	})
	svc := NewService(nil, Credentials{TikHub: "k"}, nil)
	res := svc.Call(context.Background(), freeEndpoint(VendorTikHub, `{"timeout_ms":50}`), Request{URL: srv.URL}, "id")
	if res.Err == nil || res.Err.Kind != KindTimeout || res.Billed {
		t.Fatalf("应超时且不计费：err=%v billed=%v", res.Err, res.Billed)
	}
}

// 超长响应按坏响应处理；2xx 已计费。
func TestCallOversizedResponse(t *testing.T) {
	srv, _ := fakeUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":200,"data":"`))
		_, _ = w.Write([]byte(strings.Repeat("a", maxRespBytes)))
		_, _ = w.Write([]byte(`"}`))
	})
	svc := NewService(nil, Credentials{TikHub: "k"}, nil)
	res := svc.Call(context.Background(), freeEndpoint(VendorTikHub, `{}`), Request{URL: srv.URL}, "id")
	if res.Err == nil || res.Err.Kind != KindBadResponse || !res.Billed {
		t.Fatalf("超长响应应为已计费的 bad_response：err=%v billed=%v", res.Err, res.Billed)
	}
}

func TestErrorMessagePrefersDetail(t *testing.T) {
	if got := errorMessage([]byte(`{"detail":"Token expired","message":"x"}`)); got != "Token expired" {
		t.Errorf("得到 %q", got)
	}
	if got := errorMessage([]byte(`upstream down`)); got != "upstream down" {
		t.Errorf("非 JSON 应原样截取，得到 %q", got)
	}
}

// Package entry 识别一个请求是从哪个公网入口进来的（ADR-079）。
//
// API 只有一套，但有两个公网入口：
//   - api.squyrrl.com：Cloudflare 代理 → 新加坡源站；
//   - api.squyrrl.cn：大陆服务器的 nginx 中继 → 新加坡源站（不经 Cloudflare）。
//
// 两条路在新加坡宿主 nginx 落进两个不同的 server 块。中继块只接受持中继客户端证书的连接，
// 由它写入 X-Squyrrl-Entry: cn；Cloudflare 块用 proxy_set_header 把同名头清空。
// 所以客户端自己带上这个头到不了 API —— 判据可信的前提全在 nginx 那两行，改那里要连带看这里。
//
// 用途只有一个：回包里指向其它公网服务的绝对地址（文件边缘）要跟着入口走。
// 经大陆入口进来的客户端拿到海外地址，等于把刚绕开的跨境链路又走一遍。
// 业务逻辑不得按入口分叉 —— 两个入口后面是同一个后端，选错入口只该慢一点，不该出错。
package entry

import "net/http"

// Header 由新加坡 nginx 写入，见 server/api/nginx.conf 的中继 server 块。
const Header = "X-Squyrrl-Entry"

type Entry uint8

const (
	// Global 是默认值：Cloudflare 入口、内网直连（tgbot / admin）以及任何认不出的值都落在这里。
	Global Entry = iota
	// CN 是大陆中继入口。
	CN
)

// Of 读取请求的入口。只认精确的 "cn"：拼错或多出空白一律当 Global，
// 这一侧的代价只是大陆用户拿到海外地址、慢一点，而不是拿到一个不存在的地址。
func Of(r *http.Request) Entry {
	if r.Header.Get(Header) == "cn" {
		return CN
	}
	return Global
}

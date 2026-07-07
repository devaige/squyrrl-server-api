package parser

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
)

// manifest 下发混淆 —— 刻意与碎片的 XChaCha20 混淆（ADR-042）**完全独立**：
// 不同算法、不同密钥、不同 wire 格式，两套互不关联，破一套推不出另一套（用户要求）。
//
// 目标仅为「防一眼抓包看到明文清单」（ADR-003 威胁模型：防扫描/随手窥探，不防逆向）。
// 因此 pad 不是机密——客户端本就要内置同一份才能反混淆——故意固化在前后端源码里，
// 不走 env（它一旦当机密管理反而误导，真正的机密性在此场景不可能达到）。
//
// 前后端固化的这一套逻辑必须逐字节一致：
//
//	keystream 分块 block[i] = SHA256(pad || uint32_be(i))
//	cipher    = plain XOR keystream
//	wire      = base64_std(cipher)
//
// 确定性（无 nonce）：同内容→同输出，HTTP 缓存/ETag 仍成立；XOR 对称：同函数即反混淆。
const manifestPad = "Squyrrl/parse-manifest/obf/v1/6f2a9c4e8b1d7f30a5"

// obfuscateManifest 把明文 JSON 混淆为 base64 字符串下发。
func obfuscateManifest(plain []byte) string {
	return base64.StdEncoding.EncodeToString(manifestXOR(plain))
}

// manifestXOR 用 SHA-256 计数器模式生成 keystream 与数据异或。对称：混淆=反混淆。
func manifestXOR(data []byte) []byte {
	out := make([]byte, len(data))
	seed := []byte(manifestPad)
	var block [32]byte
	var ctr [4]byte
	var counter uint32
	pos := len(block) // 置满以触发首块生成
	for i := range data {
		if pos == len(block) {
			binary.BigEndian.PutUint32(ctr[:], counter)
			h := sha256.New()
			h.Write(seed)
			h.Write(ctr[:])
			h.Sum(block[:0]) // 摘要写回 block，避免每块分配
			counter++
			pos = 0
		}
		out[i] = data[i] ^ block[pos]
		pos++
	}
	return out
}

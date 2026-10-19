package vmess

// AEAD 模式使用的盐常量，与 xray-core 逐字对齐——盐不一致即无法互通。
const (
	KDFSaltConstAuthIDEncryptionKey             = "AES Auth ID Encryption"
	KDFSaltConstAEADRespHeaderLenKey            = "AEAD Resp Header Len Key"
	KDFSaltConstAEADRespHeaderLenIV             = "AEAD Resp Header Len IV"
	KDFSaltConstAEADRespHeaderPayloadKey        = "AEAD Resp Header Key"
	KDFSaltConstAEADRespHeaderPayloadIV         = "AEAD Resp Header IV"
	KDFSaltConstVMessAEADKDF                    = "VMess AEAD KDF"
	KDFSaltConstVMessHeaderPayloadAEADKey       = "VMess Header AEAD Key"
	KDFSaltConstVMessHeaderPayloadAEADIV        = "VMess Header AEAD Nonce"
	KDFSaltConstVMessHeaderPayloadLengthAEADKey = "VMess Header AEAD Key_Length"
	KDFSaltConstVMessHeaderPayloadLengthAEADIV  = "VMess Header AEAD Nonce_Length"
)

// cmdKeySalt 是 UUID → CmdKey 派生的追加常量。
const cmdKeySalt = "c48619fe-8f02-49e0-b9e9-edf763e17e21"

// 请求头 Option 位与 Security 编码，值与协议规范一致。
const (
	OptionChunkStream         = 0x01
	OptionChunkMasking        = 0x04
	OptionGlobalPadding       = 0x08
	OptionAuthenticatedLength = 0x10
	SecurityAES128GCM         = 0x03
	SecurityChaCha20Poly1305  = 0x04
	SecurityNone              = 0x05
	RequestCommandTCP         = 0x01
	// RequestCommandMux 标记 mux 主连接：载荷通道承载 mux.cool 帧而非直达目标的
	// 字节流，与 v2fly/Xray 同值。
	RequestCommandMux = 0x03
)

// mux 主连接在 VMess 请求目标里的标记域名与端口（v2fly/Xray 同值）：目标为该
// 域名的连接按 mux 主连接处理，载荷通道交给 internal/mux 服务端。
const (
	MuxCoolHost = "v1.mux.cool"
	MuxCoolPort = 9527
)

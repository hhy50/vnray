package vless

// tls.go 为 VLESS 入站签发自签证书：证书与私钥以 PEM 文本随插件配置存库，不落磁盘。
// 自签意味着客户端必须关闭证书校验，因此这里只保证私钥强度与有效期。

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"strings"
	"time"
)

const (
	// DefaultServerName 是未指定域名时证书使用的名字（客户端 SNI 可留空）。
	DefaultServerName = "vnray.local"

	// certValidity 是自签证书有效期。剩余不足 renewBefore 时重新签发：证书过期
	// 不会立刻报错，而是在某个时刻所有新连接一起握手失败，所以必须提前续。
	certValidity = 825 * 24 * time.Hour
	renewBefore  = 30 * 24 * time.Hour
)

// Cert 是入站使用的证书及其元信息。CertPEM/KeyPEM 是当前生效的 PEM 文本，
// Renewed 为 true 时调用方必须把它们回写持久化，否则每次启动都会重签。
type Cert struct {
	// Certificate 是解析好的密钥对，直接喂给 tls.Config。
	tls.Certificate
	// ServerName 是证书覆盖的域名（客户端 SNI）。
	ServerName string
	// NotAfter 是证书到期时间。
	NotAfter time.Time
	// Fingerprint 是证书 DER 的 SHA-256（十六进制），供客户端核对。
	Fingerprint string
	// CertPEM/KeyPEM 是当前生效的证书与私钥 PEM。
	CertPEM []byte
	KeyPEM  []byte
	// Renewed 表示本次调用重新签发了证书。
	Renewed bool
}

// Load 返回可用证书：给定 PEM 能解析、覆盖 serverName 且未临近过期就直接用，否则
// 重新签发。serverName 为空取 DefaultServerName。
func Load(serverName string, certPEM, keyPEM []byte) (Cert, error) {
	name := normalizeServerName(serverName)
	if len(certPEM) > 0 && len(keyPEM) > 0 {
		if c, err := parse(certPEM, keyPEM); err == nil && time.Until(c.NotAfter) > renewBefore && c.covers(name) {
			c.ServerName = name
			return c, nil
		}
	}
	return generate(name)
}

// TLSConfig 返回入站用的 tls.Config。ALPN 同时挂 h2 与 http/1.1：客户端 alpn 参数
// 写哪个都能握手（实际承载的仍是 VLESS 明文帧，不是 HTTP）。
func (c Cert) TLSConfig() *tls.Config {
	return &tls.Config{
		Certificates: []tls.Certificate{c.Certificate},
		MinVersion:   tls.VersionTLS12,
		NextProtos:   []string{"h2", "http/1.1"},
	}
}

// covers 报告证书是否覆盖 name：改过域名就得重签。
func (c Cert) covers(name string) bool {
	leaf := c.Leaf
	if leaf == nil {
		return false
	}
	if ip := net.ParseIP(name); ip != nil {
		for _, got := range leaf.IPAddresses {
			if got.Equal(ip) {
				return true
			}
		}
		return false
	}
	for _, got := range leaf.DNSNames {
		if strings.EqualFold(got, name) {
			return true
		}
	}
	return false
}

// normalizeServerName 归一化证书名：去空白，空取默认名。
func normalizeServerName(raw string) string {
	name := strings.TrimSpace(raw)
	if name == "" {
		return DefaultServerName
	}
	return name
}

// parse 解析 PEM 证书对；返回的 Cert 带 Leaf、指纹与原始 PEM。
func parse(certPEM, keyPEM []byte) (Cert, error) {
	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return Cert{}, err
	}
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return Cert{}, err
	}
	pair.Leaf = leaf
	sum := sha256.Sum256(leaf.Raw)
	return Cert{
		Certificate: pair,
		NotAfter:    leaf.NotAfter,
		Fingerprint: hex.EncodeToString(sum[:]),
		CertPEM:     certPEM,
		KeyPEM:      keyPEM,
	}, nil
}

// generate 签发一张自签证书（ECDSA P-256）。
func generate(name string) (Cert, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return Cert{}, fmt.Errorf("vless: generate key: %w", err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return Cert{}, fmt.Errorf("vless: generate serial: %w", err)
	}
	now := time.Now()
	tpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: name, Organization: []string{"vnray self-signed"}},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(certValidity),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	if ip := net.ParseIP(name); ip != nil {
		tpl.IPAddresses = []net.IP{ip}
	} else {
		tpl.DNSNames = []string{name}
	}
	// 回环也进 SAN：调试时用 127.0.0.1 直连，名字校验不至于立刻失败。
	tpl.DNSNames = append(tpl.DNSNames, "localhost")
	tpl.IPAddresses = append(tpl.IPAddresses, net.ParseIP("127.0.0.1"), net.ParseIP("::1"))

	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		return Cert{}, fmt.Errorf("vless: sign certificate: %w", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return Cert{}, fmt.Errorf("vless: marshal key: %w", err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	c, err := parse(certPEM, keyPEM)
	if err != nil {
		return Cert{}, err
	}
	c.ServerName = name
	c.Renewed = true
	return c, nil
}

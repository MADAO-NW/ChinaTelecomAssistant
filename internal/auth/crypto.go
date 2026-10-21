// Package auth 中国电信 App 登录：手机号 + 密码（loginType=4）、
// 手机号 + 短信验证码 + 滑块（loginType=2），以及登录态的本地持久化。
//
// 两条链路都复现 App 的 content/headerInfos 请求结构与网关加密方式。
package auth

import (
	"bytes"
	"crypto/aes"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"math/big"
)

// gatewayPubKey 登录网关 RSA 公钥（PKCS#1 v1.5，1024 位）
const gatewayPubKey = `MIGfMA0GCSqGSIb3DQEBAQUAA4GNADCBiQKBgQDBkLT15ThVgz6/NOl6s8GNPofd
WzWbCkWnkaAm7O2LjkM1H7dMvzkiqdxU02jamGRHLX/ZNMCXHnPcW/sDhiFCBN18
qFvy8g6VYb9QtroI09e176s+ZCtiv7hbin2cCTj99iUpnEloZm19lwHyo69u5UMi
PMpq0/XKBO8lYhN/gwIDAQAB`

// ASCIIEncrypt App 的 ASCIIEncrypt：逐字符码点 +2（手机号、密码、验证码都要先过这一步）
func ASCIIEncrypt(s string) string {
	r := []rune(s)
	for i := range r {
		r[i] += 2
	}
	return string(r)
}

// EncodePhone 手机号的业务侧编码，等价于 ASCIIEncrypt
func EncodePhone(phone string) string {
	return ASCIIEncrypt(phone)
}

// randHex 生成 n 位小写十六进制随机串
func randHex(n int) string {
	const alphabet = "abcdef0123456789"
	b := make([]byte, n)
	for i := range b {
		v, _ := rand.Int(rand.Reader, big.NewInt(int64(len(alphabet))))
		b[i] = alphabet[v.Int64()]
	}
	return string(b)
}

// rsaEncrypt 网关 RSA 加密（PKCS#1 v1.5），输出 base64
func rsaEncrypt(msg string) (string, error) {
	block, _ := pem.Decode([]byte("-----BEGIN PUBLIC KEY-----\n" + gatewayPubKey + "\n-----END PUBLIC KEY-----"))
	if block == nil {
		return "", fmt.Errorf("公钥解析失败")
	}
	key, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return "", err
	}
	pub, ok := key.(*rsa.PublicKey)
	if !ok {
		return "", fmt.Errorf("非 RSA 公钥")
	}
	enc, err := rsa.EncryptPKCS1v15(rand.Reader, pub, []byte(msg))
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(enc), nil
}

// aesECBEncrypt AES/ECB/PKCS7 + base64（与 App 的 ESAESECBUtil 一致，滑块参数用）
func aesECBEncrypt(plain, key []byte) (string, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	bs := block.BlockSize()
	pad := bs - len(plain)%bs
	buf := append(append([]byte{}, plain...), bytes.Repeat([]byte{byte(pad)}, pad)...)
	out := make([]byte, len(buf))
	for i := 0; i < len(buf); i += bs {
		block.Encrypt(out[i:i+bs], buf[i:i+bs])
	}
	return base64.StdEncoding.EncodeToString(out), nil
}

// newUUID 生成大写 UUID，用作设备号
func newUUID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%X-%X-%X-%X-%X", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

package admin

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"crypto/subtle"
	"encoding/base32"
	"encoding/binary"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	// totpPeriodSeconds 是认证器与服务端共同使用的固定时间步长。
	totpPeriodSeconds int64 = 30
	// totpDigits 是动态验证码位数。
	totpDigits = 6
	// totpSecretBytes 是 TOTP 随机共享密钥长度。
	totpSecretBytes = 20
)

// TOTPConfig 保存已确认启用的管理员 TOTP 配置。
type TOTPConfig struct {
	Secret string `json:"secret"`
}

// GenerateTOTPSecret 生成规范化、无填充的 Base32 TOTP 密钥。
func GenerateTOTPSecret() (string, error) {
	secret := make([]byte, totpSecretBytes)
	if _, err := rand.Read(secret); err != nil {
		return "", fmt.Errorf("生成 TOTP 密钥: %w", err)
	}
	return base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(secret), nil
}

// ProvisioningURI 生成兼容常见认证器的 otpauth URI。
func ProvisioningURI(issuer, account, secret string) (string, error) {
	if issuer == "" || account == "" {
		return "", errors.New("TOTP issuer 和账号不能为空")
	}
	if _, err := decodeTOTPSecret(secret); err != nil {
		return "", err
	}
	query := url.Values{}
	query.Set("secret", secret)
	query.Set("issuer", issuer)
	query.Set("algorithm", "SHA1")
	query.Set("digits", strconv.Itoa(totpDigits))
	query.Set("period", strconv.FormatInt(totpPeriodSeconds, 10))
	return "otpauth://totp/" + url.PathEscape(issuer+":"+account) + "?" + query.Encode(), nil
}

// VerifyTOTP 校验当前及相邻时间步，并拒绝已经成功消费的时间步。
func VerifyTOTP(secret, code string, now time.Time, lastAcceptedStep int64) (int64, bool) {
	if len(code) != totpDigits {
		return 0, false
	}
	for _, character := range code {
		if character < '0' || character > '9' {
			return 0, false
		}
	}
	currentStep := now.Unix() / totpPeriodSeconds
	for _, offset := range []int64{0, -1, 1} {
		step := currentStep + offset
		if step <= lastAcceptedStep || step < 0 {
			continue
		}
		expected, err := generateTOTPCode(secret, step, totpDigits)
		if err != nil {
			return 0, false
		}
		if subtle.ConstantTimeCompare([]byte(expected), []byte(code)) == 1 {
			return step, true
		}
	}
	return 0, false
}

func validateTOTPConfig(config *TOTPConfig) error {
	if config == nil {
		return nil
	}
	_, err := decodeTOTPSecret(config.Secret)
	return err
}

func decodeTOTPSecret(secret string) ([]byte, error) {
	if secret == "" || strings.ContainsAny(secret, "= \t\r\n") {
		return nil, errors.New("TOTP 密钥格式无效")
	}
	decoded, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(secret)
	if err != nil || len(decoded) != totpSecretBytes {
		return nil, errors.New("TOTP 密钥格式无效")
	}
	if base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(decoded) != secret {
		return nil, errors.New("TOTP 密钥必须使用规范 Base32 格式")
	}
	return decoded, nil
}

func generateTOTPCode(secret string, step int64, digits int) (string, error) {
	key, err := decodeTOTPSecret(secret)
	if err != nil {
		return "", err
	}
	counter := make([]byte, 8)
	binary.BigEndian.PutUint64(counter, uint64(step))
	mac := hmac.New(sha1.New, key)
	_, _ = mac.Write(counter)
	digest := mac.Sum(nil)
	offset := digest[len(digest)-1] & 0x0f
	value := (uint32(digest[offset])&0x7f)<<24 |
		uint32(digest[offset+1])<<16 |
		uint32(digest[offset+2])<<8 |
		uint32(digest[offset+3])
	modulus := uint32(1)
	for range digits {
		modulus *= 10
	}
	return fmt.Sprintf("%0*d", digits, value%modulus), nil
}

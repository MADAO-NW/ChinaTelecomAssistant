package auth

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"time"
)

// SendSms 触发短信验证码，返回短信流水号。
//
// sign 是滑块换来的 signSignatureString；服务端要求滑块而这里没带时，
// 会返回 resultCode=122001，调用方据此把滑块界面推给用户。
func (m *Manager) SendSms(phone, key, sign string) (smsID string, err error) {
	deviceID, err := m.DeviceID()
	if err != nil {
		return "", err
	}
	r, raw, err := m.call(smsProfile(), phone, "login/client/getLoginRandomCode", map[string]string{
		"phoneNum":            ASCIIEncrypt(phone),
		"imsi":                "",
		"key":                 key,
		"validationCode":      "",
		"scene":               "55",
		"deviceUid":           ASCIIEncrypt(deviceID),
		"salesProdId":         "",
		"signSignatureString": sign,
		"payType":             "",
	})
	if err != nil {
		return "", err
	}
	if r.ResponseData.ResultCode == "122001" {
		return "", ErrNeedSlider
	}
	if r.ResponseData.ResultCode != "0000" {
		return "", fmt.Errorf("发送短信失败: %s %s (%s)", r.ResponseData.ResultCode, r.ResponseData.ResultDesc, snippet(raw))
	}
	var d struct {
		SmsID string `json:"smsId"`
	}
	_ = json.Unmarshal(r.ResponseData.Data, &d)
	return d.SmsID, nil
}

// LoginBySms 用短信验证码登录，返回登录结果。
func (m *Manager) LoginBySms(phone, code, sign string) (*LoginResult, error) {
	deviceID, err := m.DeviceID()
	if err != nil {
		return nil, err
	}
	ts := time.Now().Format("20060102150405")
	// 与 App 一致：iPhone 14 15.4.<随机串><手机号><时间戳><验证码>0$$$0.
	cipherMsg := fmt.Sprintf("iPhone 14 15.4.%s%s%s%s0$$$0.", randHex(12), phone, ts, code)
	enc, err := rsaEncrypt(cipherMsg)
	if err != nil {
		return nil, err
	}
	fd := map[string]string{
		// App 的 authentication = base64(ASCIIEncrypt(验证码))
		"accountType":                "",
		"authentication":             base64.StdEncoding.EncodeToString([]byte(ASCIIEncrypt(code))),
		"deviceUid":                  deviceID,
		"isChinatelecom":             "0",
		"loginAuthCipherAsymmertric": enc,
		"loginType":                  "2",
		"phoneNum":                   ASCIIEncrypt(phone),
		"systemVersion":              "15.4.0",
	}
	// 没有滑块签名时不写该字段，避免把空串当成已通过校验
	if sign != "" {
		fd["signSignatureString"] = sign
	}
	r, raw, err := m.call(smsProfile(), phone, "login/client/userLoginNormal", fd)
	if err != nil {
		return nil, err
	}
	return parseLoginResult(r, raw)
}

// LoginByPassword 用手机号 + 密码登录（loginType=4），即旧版 ChinaTelecomMonitor 的方式。
func (m *Manager) LoginByPassword(phone, password string) (*LoginResult, error) {
	// 设备号 = 12 位随机串 + 固定字符 4 + 3 位随机串
	deviceID := randHex(12) + "4" + randHex(3)
	ts := time.Now().Format("20060102150400")
	// iPhone 14 15.4.<随机串><手机号><时间戳><密码>0$$$0.
	cipherMsg := fmt.Sprintf("iPhone 14 15.4.%s%s%s%s0$$$0.", randHex(12), phone, ts, password)
	enc, err := rsaEncrypt(cipherMsg)
	if err != nil {
		return nil, err
	}
	r, raw, err := m.call(passwordProfile(), phone, "login/client/userLoginNormal", map[string]string{
		"accountType":                "",
		"authentication":             ASCIIEncrypt(password),
		"deviceUid":                  deviceID,
		"isChinatelecom":             "0",
		"loginAuthCipherAsymmertric": enc,
		"loginType":                  "4",
		"phoneNum":                   ASCIIEncrypt(phone),
		"systemVersion":              "15.4.0",
	})
	if err != nil {
		return nil, err
	}
	return parseLoginResult(r, raw)
}

// parseLoginResult 解析 userLoginNormal 响应，抽出 token 与归属地
func parseLoginResult(r *apiResult, raw []byte) (*LoginResult, error) {
	if r.ResponseData.ResultCode == "122001" {
		return nil, ErrNeedSlider
	}
	if r.ResponseData.ResultCode != "0000" {
		return nil, fmt.Errorf("登录失败: %s %s (%s)", r.ResponseData.ResultCode, r.ResponseData.ResultDesc, snippet(raw))
	}
	var d struct {
		LoginSuccessResult struct {
			Token        string `json:"token"`
			PhoneNbr     string `json:"phoneNbr"`
			ProvinceName string `json:"provinceName"`
			CityName     string `json:"cityName"`
		} `json:"loginSuccessResult"`
	}
	if err := json.Unmarshal(r.ResponseData.Data, &d); err != nil {
		return nil, err
	}
	res := d.LoginSuccessResult
	if res.Token == "" {
		return nil, fmt.Errorf("登录成功但 token 为空: %s", snippet(raw))
	}
	return &LoginResult{
		Token:    res.Token,
		Phone:    res.PhoneNbr,
		Province: res.ProvinceName,
		City:     res.CityName,
	}, nil
}

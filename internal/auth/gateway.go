package auth

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"ctm/configs"
)

const (
	// loginBase 登录网关
	loginBase = "https://appgologin.189.cn:9031"

	// 网关公共来源标识，App 登录接口只认这一组
	shopID         = "20002"
	source         = "110003"
	sourcePassword = "Sid98s"
)

// apiResult 登录网关统一响应
type apiResult struct {
	HeaderInfos struct {
		Code   string `json:"code"`
		Reason string `json:"reason"`
	} `json:"headerInfos"`
	ResponseData struct {
		ResultCode string          `json:"resultCode"`
		ResultDesc string          `json:"resultDesc"`
		Data       json.RawMessage `json:"data"`
	} `json:"responseData"`
}

// profile 一条登录链路使用的客户端标识与时间戳精度
type profile struct {
	clientType      string
	userAgent       string
	timestampLayout string
}

// passwordProfile 密码登录沿用旧版 v2.0.3 实测可用的参数（客户端版本取 -clientVersion）
func passwordProfile() profile {
	return profile{
		clientType:      "#" + configs.ClientVersion + "#channel50#iPhone 14 Pro#",
		timestampLayout: "20060102150400",
	}
}

// smsProfile 滑块与短信登录沿用 CTPocket 13.4.0 实测可用的参数
func smsProfile() profile {
	return profile{
		clientType:      "#13.4.0#channel50#iPhone 14 Pro#",
		userAgent:       "CTPocket/13.4.0 (iPhone; iOS 17.0; Scale/3.00)",
		timestampLayout: "20060102150405",
	}
}

// call 发送 App 统一的 content/headerInfos 请求，返回解析后的响应与原始 body
func (m *Manager) call(p profile, phone, path string, fieldData map[string]string) (*apiResult, []byte, error) {
	header := map[string]string{
		"code":           path[strings.LastIndex(path, "/")+1:],
		"timestamp":      time.Now().Format(p.timestampLayout),
		"clientType":     p.clientType,
		"shopId":         shopID,
		"source":         source,
		"sourcePassword": sourcePassword,
		"userLoginName":  ASCIIEncrypt(phone),
	}
	body, err := json.Marshal(map[string]any{
		"content": map[string]any{
			"fieldData": fieldData,
			"attach":    "iPhone",
		},
		"headerInfos": header,
	})
	if err != nil {
		return nil, nil, err
	}

	req, err := http.NewRequest(http.MethodPost, loginBase+"/"+path, bytes.NewReader(body))
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("Content-Type", "application/json; charset=UTF-8")
	req.Header.Set("Accept", "application/json")
	if p.userAgent != "" {
		req.Header.Set("User-Agent", p.userAgent)
	}

	cli := &http.Client{Timeout: time.Duration(configs.TimeOut) * time.Second}
	resp, err := cli.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)

	var r apiResult
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, raw, fmt.Errorf("响应不是 JSON: %s", snippet(raw))
	}
	return &r, raw, nil
}

// snippet 截断原始响应，避免把整段报文塞进错误信息或日志
func snippet(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 300 {
		return s[:300] + "…"
	}
	return s
}

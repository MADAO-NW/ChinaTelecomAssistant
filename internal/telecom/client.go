// Package telecom 中国电信业务网关：话费/流量/语音查询，token 过期时按需重登。
package telecom

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"ctm/configs"
	"ctm/internal/auth"
	"ctm/models"
)

const (
	// fuwuBase 业务网关
	fuwuBase = "https://appfuwu.189.cn:9021"

	// 业务网关公共来源标识
	shopID         = "20002"
	source         = "110003"
	sourcePassword = "Sid98s"

	// tokenExpired 上游判定 token 失效的 headerInfos.code
	tokenExpired = "X201"
)

// Client 业务接口客户端，token 由登录管理器提供
type Client struct {
	auth *auth.Manager
	// lastPasswordLogin 上次密码登录时间，用于登录节流
	lastPasswordLogin time.Time
}

// NewClient 创建业务客户端
func NewClient(m *auth.Manager) *Client {
	return &Client{auth: m}
}

// call 发送业务网关请求；autoLogin 为 true 时 token 失效会先重新登录再重试一次
func (c *Client) call(code, path string, fieldData map[string]string, out any) ([]byte, error) {
	phone, token := c.auth.State()
	if phone == "" || token == "" {
		return nil, errors.New("尚未登录，请先在网页上完成登录")
	}
	raw, err := c.do(code, path, phone, token, fieldData)
	if err != nil {
		return raw, err
	}
	// token 过期：有账号密码时自动重登并重试一次，否则提示重新登录
	if resultCode(raw) == tokenExpired {
		if configs.Username == "" || configs.Password == "" {
			return raw, errors.New("token 已过期，请重新登录")
		}
		configs.Logger.Warnw("token 已过期，使用保存的账号密码重新登录", "code", code)
		if err := c.relogin(); err != nil {
			return raw, err
		}
		_, token = c.auth.State()
		raw, err = c.do(code, path, phone, token, fieldData)
		if err != nil {
			return raw, err
		}
	}
	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			return raw, fmt.Errorf("响应解析失败: %w", err)
		}
	}
	return raw, nil
}

// do 单次业务请求
func (c *Client) do(code, path, phone, token string, fieldData map[string]string) ([]byte, error) {
	body, err := json.Marshal(map[string]any{
		"content": map[string]any{
			"fieldData": fieldData,
			"attach":    "test",
		},
		"headerInfos": map[string]string{
			"clientType":     "#" + configs.ClientVersion + "#channel50#iPhone 14 Pro#",
			"code":           code,
			"shopId":         shopID,
			"source":         source,
			"sourcePassword": sourcePassword,
			"timestamp":      time.Now().Format("20060102150400"),
			"userLoginName":  auth.EncodePhone(phone),
			"token":          token,
		},
	})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequest(http.MethodPost, fuwuBase+"/"+path, strings.NewReader(string(body)))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json; charset=UTF-8")
	req.Header.Set("Connection", "Keep-Alive")

	resp, err := (&http.Client{Timeout: time.Duration(configs.TimeOut) * time.Second}).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	configs.Logger.Infow("业务接口返回", "code", code, "path", path, "result", string(raw))
	return raw, nil
}

// relogin 使用配置里的账号密码重新登录，遵守登录间隔限制
func (c *Client) relogin() error {
	if interval := time.Duration(configs.LoginIntervalTime) * time.Second; interval > 0 {
		if next := c.lastPasswordLogin.Add(interval); time.Now().Before(next) {
			return fmt.Errorf("%d 秒内最多登录一次，上次登录于 %s",
				configs.LoginIntervalTime, c.lastPasswordLogin.Format("2006-01-02 15:04:05"))
		}
	}
	res, err := c.auth.LoginByPassword(configs.Username, configs.Password)
	if err != nil {
		return err
	}
	c.lastPasswordLogin = time.Now()
	return c.auth.StoreToken(res, configs.Username)
}

// resultCode 取 headerInfos.code
func resultCode(raw []byte) string {
	var r struct {
		HeaderInfos struct {
			Code string `json:"code"`
		} `json:"headerInfos"`
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		return ""
	}
	return r.HeaderInfos.Code
}

// QryImportantData 话费/流量/语音汇总，数据源 /query/qryImportantData
func (c *Client) QryImportantData() (*models.Result[models.ImportantData], error) {
	var r models.Result[models.ImportantData]
	_, err := c.call("qryImportantData", "query/qryImportantData", map[string]string{
		"provinceCode":   "600101",
		"cityCode":       "8441900",
		"shopId":         shopID,
		"isChinatelecom": "0",
		"account":        auth.EncodePhone(mustPhone(c)),
	}, &r)
	if err != nil {
		return nil, err
	}
	return &r, nil
}

// UserFluxPackage 原始流量包明细，数据源 /query/userFluxPackage
func (c *Client) UserFluxPackage() (*models.Result[models.UserFluxPackageData], error) {
	var r models.Result[models.UserFluxPackageData]
	_, err := c.call("userFluxPackage", "query/userFluxPackage", map[string]string{
		"queryFlag":  "0",
		"accessAuth": "1",
		"account":    auth.EncodePhone(mustPhone(c)),
	}, &r)
	if err != nil {
		return nil, err
	}
	return &r, nil
}

// mustPhone 取当前手机号；调用前已确认登录态存在
func mustPhone(c *Client) string {
	phone, _ := c.auth.State()
	return phone
}

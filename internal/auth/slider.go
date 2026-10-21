package auth

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"time"
)

// Challenge 一次滑块挑战：背景图与拼图都是服务端直接给的 base64 PNG
type Challenge struct {
	Key    string `json:"key"`
	Extra  string `json:"extra"`
	BG     string `json:"bg"`
	Slider string `json:"slider"`
	Size   string `json:"size"`
	Scene  string `json:"scene"`
}

// FetchChallenge 取登录场景（scene=1）的滑块图
func (m *Manager) FetchChallenge(phone string) (*Challenge, error) {
	return m.fetchChallenge(phone, "1")
}

// fetchChallenge 取指定业务场景的滑块图；scene 决定后续业务短信的类型
func (m *Manager) fetchChallenge(phone, scene string) (*Challenge, error) {
	deviceID, err := m.DeviceID()
	if err != nil {
		return nil, err
	}
	r, raw, err := m.call(smsProfile(), phone, "login/client/getSliderVerificationPicture", map[string]string{
		"account":    ASCIIEncrypt(phone),
		"deviceUid":  ASCIIEncrypt(deviceID),
		"clientType": smsProfile().clientType,
		"scene":      scene,
	})
	if err != nil {
		return nil, err
	}
	if r.ResponseData.ResultCode != "0000" {
		return nil, fmt.Errorf("取滑块失败: %s %s (%s)", r.ResponseData.ResultCode, r.ResponseData.ResultDesc, snippet(raw))
	}
	var d struct {
		Key           string `json:"key"`
		Extra         string `json:"extra"`
		BackgroundImg string `json:"backgroundImg"`
		SliderImg     string `json:"sliderImg"`
		Size          string `json:"size"`
	}
	if err := json.Unmarshal(r.ResponseData.Data, &d); err != nil {
		return nil, err
	}
	return &Challenge{Key: d.Key, Extra: d.Extra, BG: d.BackgroundImg, Slider: d.SliderImg, Size: d.Size, Scene: scene}, nil
}

// VerifySlider 提交滑动结果，换取 signSignatureString
//
// distance 是相对滑动比例（0~1），取值为 left / (图宽 - 拼图宽)；
// track 是轨迹串 "x#y#t，x#y#t"，durationMs 是整段滑动的耗时。
func (m *Manager) VerifySlider(phone string, ch *Challenge, distance float64, track string, durationMs int64) (string, error) {
	deviceID, err := m.DeviceID()
	if err != nil {
		return "", err
	}
	key, err := base64.StdEncoding.DecodeString(ch.Extra)
	if err != nil {
		return "", fmt.Errorf("extra 不是合法 base64: %w", err)
	}
	enc := func(v string) string {
		out, _ := aesECBEncrypt([]byte(v), key)
		return out
	}
	if durationMs <= 0 {
		durationMs = 320
	}
	now := time.Now().UnixMilli()
	start := now - durationMs

	r, raw, err := m.call(smsProfile(), phone, "login/client/verificationSliderPicture", map[string]string{
		"account":      ASCIIEncrypt(phone),
		"deviceUid":    ASCIIEncrypt(deviceID),
		"distance":     enc(fmt.Sprintf("%.4f", distance)),
		"startTime":    enc(fmt.Sprintf("%.0f", float64(start))),
		"endTime":      enc(fmt.Sprintf("%.0f", float64(now))),
		"slidingTrack": enc(track),
		"key":          ch.Key,
		"clientType":   smsProfile().clientType,
		"shopId":       shopID,
		"scene":        ch.Scene,
	})
	if err != nil {
		return "", err
	}
	if r.ResponseData.ResultCode != "0000" {
		return "", fmt.Errorf("滑块校验失败: %s %s (%s)", r.ResponseData.ResultCode, r.ResponseData.ResultDesc, snippet(raw))
	}
	var d struct {
		SignSignatureString string `json:"signSignatureString"`
	}
	_ = json.Unmarshal(r.ResponseData.Data, &d)
	if d.SignSignatureString == "" {
		return "", fmt.Errorf("校验通过但 signSignatureString 为空: %s", snippet(raw))
	}
	return d.SignSignatureString, nil
}

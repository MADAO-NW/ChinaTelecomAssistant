package telecom

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/golang-module/carbon/v2"

	"ctm/models"
)

// ToInt64 把「1.2GB」「300MB」「1024KB」这类文本流量换算成 KB
func ToInt64(s string) (int64, error) {
	switch {
	case strings.Contains(s, "GB"):
		n, err := strconv.ParseFloat(strings.ReplaceAll(s, "GB", ""), 64)
		if err != nil {
			return 0, err
		}
		return int64(n * 1024 * 1024), nil
	case strings.Contains(s, "MB"):
		n, err := strconv.ParseFloat(strings.ReplaceAll(s, "MB", ""), 64)
		if err != nil {
			return 0, err
		}
		return int64(n * 1024), nil
	case strings.Contains(s, "KB"):
		n, err := strconv.ParseFloat(strings.ReplaceAll(s, "KB", ""), 64)
		if err != nil {
			return 0, err
		}
		return int64(n), nil
	case strings.Contains(s, "B"):
		return 0, nil
	}
	return 0, errors.New("文本格式不符合规范")
}

// ToSummary 把电信 qryImportantData 响应换算成 Summary；
// 未取到数据或上游返回异常时返回 error，由调用方决定是否覆盖已有快照。
func ToSummary(data *models.Result[models.ImportantData], username string, t carbon.Carbon) (models.Summary, error) {
	if data == nil {
		return models.Summary{}, errors.New("未获取到流量数据")
	}
	if data.HeaderInfos.Code != "0000" || data.ResponseData.ResultCode != "0000" {
		return models.Summary{}, fmt.Errorf("上游返回异常 headerInfos.code=%s resultCode=%s",
			data.HeaderInfos.Code, data.ResponseData.ResultCode)
	}
	info := data.ResponseData.Data

	useFlow, _ := strconv.ParseInt(info.FlowInfo.TotalAmount.Used, 10, 64)
	balanceFlow, _ := strconv.ParseInt(info.FlowInfo.TotalAmount.Balance, 10, 64)

	generalUse, _ := strconv.ParseInt(info.FlowInfo.CommonFlow.Used, 10, 64)
	generalBalance, _ := strconv.ParseInt(info.FlowInfo.CommonFlow.Balance, 10, 64)

	specialUse, _ := strconv.ParseInt(info.FlowInfo.SpecialAmount.Used, 10, 64)
	specialBalance, _ := strconv.ParseInt(info.FlowInfo.SpecialAmount.Balance, 10, 64)

	voiceUsage, _ := strconv.ParseInt(info.VoiceInfo.VoiceDataInfo.Used, 10, 64)
	voiceAmount, _ := strconv.ParseInt(info.VoiceInfo.VoiceDataInfo.Total, 10, 64)

	balanceFloat, _ := strconv.ParseFloat(info.BalanceInfo.IndexBalanceDataInfo.Balance, 64)
	// 上游余额是元字符串，接口按分输出；直接截断会少 1 分，需四舍五入
	balance := int64(math.Round(balanceFloat * 100))

	var items []models.SummaryItems
	for _, flow := range info.FlowInfo.FlowList {
		if !strings.Contains(flow.Title, "流量") {
			continue
		}
		var use, left int64
		if strings.Contains(flow.LeftTitle, "已用") {
			use, _ = ToInt64(flow.LeftTitleHh)
		}
		if strings.Contains(flow.RightTitle, "剩余") {
			left, _ = ToInt64(flow.RightTitleHh)
		}
		items = append(items, models.SummaryItems{
			Name:  flow.Title,
			Use:   use,
			Total: use + left,
		})
	}

	return models.Summary{
		Username:     username,
		Use:          useFlow,
		Total:        useFlow + balanceFlow,
		GeneralUse:   generalUse,
		GeneralTotal: generalUse + generalBalance,
		SpecialUse:   specialUse,
		SpecialTotal: specialUse + specialBalance,
		Balance:      balance,
		VoiceUsage:   voiceUsage,
		VoiceAmount:  voiceAmount,
		CreateTime:   carbon.DateTime{Carbon: t},
		Items:        items,
	}, nil
}

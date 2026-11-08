package payment

// QYDION 微信支付 v3 退款（交接报告 §E）
// 直接复用 wechat_native.go 的 APIv3 客户端设施：商户私钥签名（doSignedRequest）+
// 应答验签（平台证书，request 内强制）+ Enabled/requiredOff 配置门控。
// 证书/私钥本来就是同一商户号体系，零新增设施即可发起 v3 退款，因此按交接要求
// 实做而非留 TODO；未配置时与其他渠道同样返回明确错误。

import (
	"errors"
	"fmt"
	"strings"
)

// wxRefundPath 申请退款（申请商户退款 API）
const wxRefundPath = "/v3/refund/domestic/refunds"

func (p *WxNativeProvider) Refund(req RefundRequest) (RefundResult, error) {
	if !p.Enabled() {
		return RefundResult{}, p.requiredOff()
	}
	if req.RefundCents <= 0 {
		return RefundResult{}, errors.New("退款金额必须大于 0")
	}
	if req.TradeNo == "" || req.OutRequestNo == "" {
		return RefundResult{}, errors.New("缺少原订单号或退款单号")
	}
	if req.TotalCents <= 0 || req.RefundCents > req.TotalCents {
		return RefundResult{}, errors.New("微信退款金额非法（须为正且不超过原单实付）")
	}
	body := map[string]any{
		"out_trade_no":  req.TradeNo,
		"out_refund_no": req.OutRequestNo,
		"amount": map[string]any{
			"refund":   req.RefundCents,
			"total":    req.TotalCents,
			"currency": "CNY",
		},
	}
	if reason := strings.TrimSpace(req.Reason); reason != "" {
		runes := []rune(reason)
		if len(runes) > 80 {
			runes = runes[:80] // 微信 reason 上限 80 字节语义；截断防整包被拒
		}
		body["reason"] = string(runes)
	}
	raw, err := p.request("POST", wxRefundPath, body)
	if err != nil {
		return RefundResult{}, err // HTTP 非 2xx / 验签失败 / 网络错误：一律按失败，不得假成功
	}
	return wxRefundOutcome(raw)
}

// wxRefundOutcome 解析 v3 退款应答，判定结果与凭证号（纯函数，单测覆盖）。
//
//	SUCCESS：退款完成。
//	PROCESSING：渠道已受理、资金退回异步进行。按「受理成功」记账（凭证 refund_id），
//	终态以微信侧为准——若渠道最终拒付（ABNORMAL），金额差异走人工对账；
//	绝不因等待异步终态而把额度长期悬在 pending，也不凭空反向补额度。
//	CLOSED / ABNORMAL：受理失败（如商户余额不足），按失败处理。
//	其他/缺失状态：按未知失败，绝不假成功。
func wxRefundOutcome(raw map[string]any) (RefundResult, error) {
	if raw == nil {
		return RefundResult{}, errors.New("微信退款应答为空")
	}
	refundId, _ := raw["refund_id"].(string)
	status, _ := raw["status"].(string)
	switch status {
	case "SUCCESS", "PROCESSING":
		return RefundResult{Succeeded: true, Credential: refundId, ChannelStatus: status}, nil
	case "CLOSED", "ABNORMAL":
		return RefundResult{}, fmt.Errorf("微信退款未成功 status=%s refund_id=%s", status, refundId)
	default:
		return RefundResult{}, fmt.Errorf("微信退款应答状态未知 status=%s（不按成功处理）", status)
	}
}

package payment

// QYDION 支付宝退款（alipay.trade.refund，交接报告 §E）
// 复用 alipay_f2f.go 的全部签名/网关/响应验签设施（call → AlipaySign / AlipayResponseNode，
// 请求侧含 sign_type、响应侧验签），不新增依赖、不另起一套规范。
// 未配置密钥时 Enabled()=false，Refund 返回明确错误；绝不半成品假成功。

import (
	"errors"
	"fmt"
)

// Refund 实现 alipay.trade.refund：
//
//	out_trade_no=原订单号、out_request_no=退款单号（全局唯一，本系统生成）、
//	refund_amount=退款金额（元，两位小数，由分换算）。
//
// 同步接口：code=10000 即退款受理成功，凭证取响应里的支付宝交易号 trade_no。
func (p *AlipayF2FProvider) Refund(req RefundRequest) (RefundResult, error) {
	if !p.Enabled() {
		return RefundResult{}, p.requiredOff()
	}
	if req.RefundCents <= 0 {
		return RefundResult{}, errors.New("退款金额必须大于 0")
	}
	if req.TradeNo == "" || req.OutRequestNo == "" {
		return RefundResult{}, errors.New("缺少原订单号或退款单号")
	}
	biz := map[string]any{
		"out_trade_no":   req.TradeNo,
		"out_request_no": req.OutRequestNo,
		"refund_amount":  fmt.Sprintf("%.2f", float64(req.RefundCents)/100),
	}
	if reason := req.Reason; reason != "" {
		biz["refund_reason"] = reason
	}
	resp, err := p.call("alipay.trade.refund", biz, "")
	if err != nil {
		return RefundResult{}, err
	}
	return alipayRefundOutcome(resp)
}

// alipayRefundOutcome 从已验签的响应里判定退款结果（纯函数，单测覆盖）。
//
//	code=10000：受理成功。fund_change=N 表示本次未产生新的资金变动（同
//	out_request_no 重放的幂等应答）；本系统 out_request_no 唯一，正常不会出现，
//	出现说明渠道侧已按同号受理过——仍按成功记录凭证号，交人工对账，不得反向补额度。
//	其他 code：受理失败（含 ACQ.REFUND_AMT_INCONSISTENT 超可退等），调用方按 failed 处理。
func alipayRefundOutcome(resp map[string]any) (RefundResult, error) {
	node, _ := resp["alipay_trade_refund_response"].(map[string]any)
	if node == nil {
		return RefundResult{}, errors.New("支付宝退款响应缺失业务节点")
	}
	code, _ := node["code"].(string)
	if code != "10000" {
		subCode, _ := node["sub_code"].(string)
		subMsg, _ := node["sub_msg"].(string)
		return RefundResult{}, fmt.Errorf("支付宝退款失败 code=%s sub_code=%s %s", code, subCode, subMsg)
	}
	credential, _ := node["trade_no"].(string)
	return RefundResult{Succeeded: true, Credential: credential, ChannelStatus: "success"}, nil
}

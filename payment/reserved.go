package payment

// QYDION 预留支付渠道（§8.6b 渠道规划：占位原则 §0.8 —— 代码结构在、配置留空、开关关闭）
// - wx_native：微信 Native 扫码（需商户号 + APIv3 平台证书；用户确认有商户号后按接口补实现）
// - epay：易支付协议已有原生实现（model/topup.go RechargeEpay + go-epay），此处仅做注册占位，
//   保证后台「支付设置」渠道清单完整；实际流量走原生代码。
// - intl_*：国际平台（Stripe 已有原生实现；Paddle/LS 等）——用户注册后告知，实现接口即可插入。

import (
	"errors"
	"net/http"
)

type ReservedProvider struct {
	name        string
	displayName string
}

func (p *ReservedProvider) Name() string        { return p.name }
func (p *ReservedProvider) DisplayName() string { return p.displayName }
func (p *ReservedProvider) Enabled() bool       { return false }

func (p *ReservedProvider) CreatePayment(order Order) (PaymentRequest, error) {
	return PaymentRequest{}, errors.New("渠道预留中：未签约，配置为空（占位原则 §0.8）")
}

func (p *ReservedProvider) HandleCallback(req *http.Request) (ChannelReport, error) {
	return ChannelReport{}, errors.New("渠道预留中：未签约")
}

func (p *ReservedProvider) QueryStatus(order Order) (ChannelReport, error) {
	return ChannelReport{}, errors.New("渠道预留中：未签约")
}

func init() {
	// M7：wx_native / stripe 已全量实做（wechat_native.go / stripe_checkout.go），不再占位。
	Register(&ReservedProvider{name: "intl_paddle", displayName: "Paddle（国际线预留）"})
	Register(&ReservedProvider{name: "intl_lemonsqueezy", displayName: "LemonSqueezy（国际线预留）"})
	// epay 实际流量走原生实现（model/topup.go RechargeEpay + go-epay），此处仅注册展示：
	Register(&ReservedProvider{name: "epay", displayName: "易支付（原生实现承载）"})
}

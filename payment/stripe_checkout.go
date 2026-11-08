package payment

// QYDION Stripe Checkout（intl 线，§8.6b M7 全量实做，替代原预留占位）
// 复用上游已有依赖 github.com/stripe/stripe-go/v81（不新增依赖，§0.2）。
// 后台选项（填入即启用）：
//   QydionStripeSecretKey     sk_live_/sk_test_ 密钥
//   QydionStripeWebhookSecret whsec_ 签名密钥
//   QydionStripeCurrency      计价币种（默认 usd；stripe 不支持 CNY 收单）
//   QydionStripeSuccessUrl / QydionStripeCancelUrl（留空则由 controller 用站点地址拼接）
// 金额口径：订单为人民币（Money），按 operation_setting.Price 折美元计价；
// 回调金额回折后与订单比对（±0.05 元容差，吸收分位取整）。

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/stripe/stripe-go/v81"
	stripeclient "github.com/stripe/stripe-go/v81/client"
	"github.com/stripe/stripe-go/v81/webhook"
)

// StripeProvider 无状态：checkout session id 与回执金额一律随返回值交出。
type StripeProvider struct{}

func (p *StripeProvider) Name() string        { return "stripe" }
func (p *StripeProvider) DisplayName() string { return "Stripe Checkout（国际卡支付）" }

func (p *StripeProvider) Enabled() bool {
	return service.QydionSettingStr("QydionStripeSecretKey", "") != "" &&
		service.QydionSettingStr("QydionStripeWebhookSecret", "") != ""
}

func (p *StripeProvider) requiredOff() error {
	return errors.New("Stripe 未配置参数（QydionStripeSecretKey/WebhookSecret），无法发起支付")
}

func stripeWebhookSecret() string { return service.QydionSettingStr("QydionStripeWebhookSecret", "") }

func (p *StripeProvider) client() *stripeclient.API {
	sc := &stripeclient.API{}
	sc.Init(service.QydionSettingStr("QydionStripeSecretKey", ""), nil)
	return sc
}

// usdCents 人民币 → 计价币种最小单位（美元分）。Price：1 USD = Price CNY。
func stripeMinorUnits(moneyCNY float64) (int64, string, error) {
	price := operation_setting.Price
	if price <= 0 {
		return 0, "", errors.New("充值单价（Price）未配置，无法折算美元计价")
	}
	currency := service.QydionSettingStr("QydionStripeCurrency", "usd")
	if currency == "cny" {
		return int64(moneyCNY*100 + 0.5), currency, nil
	}
	usd := moneyCNY / price
	return int64(usd*100 + 0.5), currency, nil
}

// minorUnitsToCNY 回折人民币（金额比对用）
func minorUnitsToCNY(units int64, currency string) float64 {
	if currency == "cny" {
		return float64(units) / 100
	}
	return (float64(units) / 100) * operation_setting.Price
}

// CreatePayment Checkout Session → 返回跳转 URL（payUrl）与会话引用（session id）
func (p *StripeProvider) CreatePayment(order Order) (PaymentRequest, error) {
	if !p.Enabled() {
		return PaymentRequest{}, p.requiredOff()
	}
	units, currency, err := stripeMinorUnits(order.Money)
	if err != nil || units <= 0 {
		return PaymentRequest{}, fmt.Errorf("Stripe 计价失败：%v", err)
	}
	successURL := service.QydionSettingStr("QydionStripeSuccessUrl", "")
	if successURL == "" {
		successURL = order.ReturnURL
	}
	cancelURL := service.QydionSettingStr("QydionStripeCancelUrl", "")
	if cancelURL == "" {
		cancelURL = order.ReturnURL
	}
	params := &stripe.CheckoutSessionParams{
		Mode:              stripe.String("payment"),
		ClientReferenceID: stripe.String(order.TradeNo),
		SuccessURL:        stripe.String(successURL),
		CancelURL:         stripe.String(cancelURL),
		ExpiresAt:         stripe.Int64(time.Now().Add(30 * time.Minute).Unix()),
		Metadata:          map[string]string{"trade_no": order.TradeNo},
		PaymentIntentData: &stripe.CheckoutSessionPaymentIntentDataParams{Metadata: map[string]string{"trade_no": order.TradeNo}},
	}
	params.LineItems = []*stripe.CheckoutSessionLineItemParams{
		{
			Quantity: stripe.Int64(1),
			PriceData: &stripe.CheckoutSessionLineItemPriceDataParams{
				Currency:   stripe.String(currency),
				UnitAmount: stripe.Int64(units),
				ProductData: &stripe.CheckoutSessionLineItemPriceDataProductDataParams{
					Name: stripe.String(order.Subject),
				},
			},
		},
	}
	s, err := p.client().CheckoutSessions.New(params)
	if err != nil {
		return PaymentRequest{}, fmt.Errorf("Stripe 创建会话失败：%v", err)
	}
	return PaymentRequest{PayUrl: s.URL, ProviderRef: s.ID}, nil
}

// HandleCallback webhook 验签（HMAC-SHA256，官方库）→ checkout.session.completed
func (p *StripeProvider) HandleCallback(req *http.Request) (ChannelReport, error) {
	if !p.Enabled() {
		return ChannelReport{}, p.requiredOff()
	}
	const maxBody = 1 << 20
	body, err := io.ReadAll(io.LimitReader(req.Body, maxBody))
	if err != nil {
		return ChannelReport{}, err
	}
	event, err := webhook.ConstructEventWithOptions(body, req.Header.Get("Stripe-Signature"),
		stripeWebhookSecret(), webhook.ConstructEventOptions{IgnoreAPIVersionMismatch: true})
	if err != nil {
		return ChannelReport{}, fmt.Errorf("Stripe webhook 验签失败：%v", err)
	}
	if event.Type != "checkout.session.completed" && event.Type != "checkout.session.async_payment_succeeded" {
		return ChannelReport{}, nil // 非完成事件：忽略（不视为错误）
	}
	var s stripe.CheckoutSession
	if err := json.Unmarshal(event.Data.Raw, &s); err != nil {
		return ChannelReport{}, fmt.Errorf("Stripe 事件解析失败：%v", err)
	}
	report := ChannelReport{
		OrderId: s.ClientReferenceID,
		Paid:    s.PaymentStatus == "paid" || s.PaymentStatus == "",
	}
	if s.AmountTotal != 0 {
		// 回折人民币，由 controller 与订单金额比对（容差吸收币种取整）
		report.AmountCNY = minorUnitsToCNY(s.AmountTotal, string(s.Currency))
	}
	return report, nil
}

// QueryStatus 主动查会话（需 order.ProviderRef = session id，controller 落库）
func (p *StripeProvider) QueryStatus(order Order) (ChannelReport, error) {
	if !p.Enabled() {
		return ChannelReport{}, p.requiredOff()
	}
	if order.ProviderRef == "" {
		return ChannelReport{}, nil // 无会话引用：以 webhook 为准
	}
	s, err := p.client().CheckoutSessions.Get(order.ProviderRef, nil)
	if err != nil {
		return ChannelReport{}, fmt.Errorf("Stripe 查询会话失败：%v", err)
	}
	report := ChannelReport{
		OrderId:   order.TradeNo,
		Paid:      string(s.PaymentStatus) == "paid",
		AmountCNY: 0,
	}
	if s.AmountTotal != 0 {
		report.AmountCNY = minorUnitsToCNY(s.AmountTotal, string(s.Currency))
	}
	return report, nil
}

func init() {
	Register(&StripeProvider{})
}

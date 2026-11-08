package payment

// QYDION 支付渠道抽象（规格书 §8.6b）
// 所有渠道（对公/支付宝当面付/微信 Native/易支付/国际平台）实现同一接口，
// 后台「支付设置」按渠道配置（密钥掩码存储，§10.2）；未签约渠道保持禁用+空配置（占位原则 §0.8）。
// 原生 epay/stripe/creem/waffo 流量继续走原生代码；QYDION 渠道从这里接入。

import (
	"net/http"
	"sort"
	"sync"
)

// Provider 支付渠道接口（§8.6b）
type Provider interface {
	Name() string // "corporate" "alipay_f2f" "wx_native" "epay" "stripe" "intl_xxx"
	DisplayName() string
	// CreatePayment 发起支付：返回支付跳转 URL、二维码内容（对公返回空，由前端展示收款账户）
	// 与渠道侧引用（ProviderRef，由 controller 落库 TopUp.ProviderRef 供查单兜底）
	CreatePayment(order Order) (PaymentRequest, error)
	// HandleCallback 处理异步回调；内部必须验签（§10.4），验签失败必须返回错误
	HandleCallback(req *http.Request) (ChannelReport, error)
	// QueryStatus 主动查询订单状态（对账兜底 §8.6b）；同样返回渠道侧实收金额，
	// 使入账前的金额比对在回调与查单两条路径上保持一致
	QueryStatus(order Order) (ChannelReport, error)
	Enabled() bool // 启用开关（后台配置；未签约渠道恒 false）
}

// PaymentRequest 渠道下单结果。渠道侧引用必须随返回值交出：
// Provider 实例由 init() 注册为全局单例，任何写在实例字段上的「最近一笔」数据都会跨请求串味。
type PaymentRequest struct {
	PayUrl      string
	QrContent   string
	ProviderRef string
}

// RefundRequest 在线退款请求（交接报告 §E）。
type RefundRequest struct {
	TradeNo      string // 原订单号（out_trade_no）
	OutRequestNo string // 退款单号（支付宝 out_request_no / 微信 out_refund_no，须全局唯一）
	RefundCents  int64  // 本次退款金额（分），>0
	TotalCents   int64  // 原订单实付总额（分；微信 v3 退款 amount.total 必填）
	Reason       string // 退款原因（可选，渠道展示用）
}

// RefundResult 渠道退款回执。凭证号随返回值交出，不得写在 Provider 实例字段上
// （全局单例跨请求共享，写了会串味）。
type RefundResult struct {
	Succeeded     bool   // 渠道是否受理成功
	Credential    string // 渠道退款凭证号（支付宝交易号 / 微信 refund_id）
	ChannelStatus string // 渠道原始状态（success / PROCESSING / SUCCESS 等）
}

// Refunder 可选能力接口：支持在线退款的渠道实现它。
// controller 用类型断言探测，未实现的渠道明确报错，而不是假装退款成功。
type Refunder interface {
	Refund(req RefundRequest) (RefundResult, error)
}

// ChannelReport 渠道对某一笔订单的回执，回调与主动查单共用同一结构。
// AmountCNY 是渠道侧口径的实收金额（人民币元）；渠道未提供时保持 0，
// 由调用方按「无金额可比对」处理，不得拿上一笔订单的金额顶上。
type ChannelReport struct {
	OrderId   string
	Paid      bool
	AmountCNY float64
}

// Order 渠道无关的订单视图（来自 model.TopUp）
type Order struct {
	TradeNo string
	UserId  int
	// Money 实付金额（人民币元；国际渠道按其 Provider 内部换算）
	Money float64
	// Amount 原生订单的美元数量（epay 语义）
	Amount    int64
	Subject   string
	NotifyURL string
	ReturnURL string
	// ProviderRef 渠道侧引用（M7：Stripe checkout session id；由 controller 落库 TopUp.ProviderRef）
	ProviderRef string
}

var (
	registryMu sync.RWMutex
	registry   = map[string]Provider{}
)

// Register 注册渠道（重复注册覆盖）
func Register(p Provider) {
	registryMu.Lock()
	defer registryMu.Unlock()
	registry[p.Name()] = p
}

// Get 按名称取渠道；不存在返回 nil
func Get(name string) Provider {
	registryMu.RLock()
	defer registryMu.RUnlock()
	return registry[name]
}

// ListAll 全部已注册渠道（后台支付设置页用，按名称排序）
func ListAll() []Provider {
	registryMu.RLock()
	defer registryMu.RUnlock()
	list := make([]Provider, 0, len(registry))
	for _, p := range registry {
		list = append(list, p)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Name() < list[j].Name() })
	return list
}

package payment

// QYDION 对公转账渠道（§8.6c）：人工审核流。
// 无在线回调：用户上传凭证 → 订单进入 reviewing → 管理员审核通过后走
// service.QydionCreditCorporateOrder 统一入账。驳回必填原因（用户可见）。

import (
	"net/http"

	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting/operation_setting"
)

type CorporateProvider struct{}

func (c *CorporateProvider) Name() string        { return "corporate" }
func (c *CorporateProvider) DisplayName() string { return "对公转账（人工审核）" }

// Enabled 对公下单必须同时满足两条：后台显式开启 QydionCorporateEnabled，且原生支付合规确认已通过。
// 默认关闭是刻意的：对公不依赖第三方签约，早期版本据此把 Enabled() 硬编码成 true，
// 结果零配置即对全部注册用户开放人工审核队列（5MB 凭证 × 20 单/天，且无清理任务），
// 小容量机器的磁盘与审核人力都会被空单灌满。
func (c *CorporateProvider) Enabled() bool {
	return service.QydionSettingBool(service.QydionKeyCorporateEnabled, false) &&
		operation_setting.IsPaymentComplianceConfirmed()
}

// CreatePayment 对公无在线支付页：返回空，前端展示收款账户 + 凭证上传表单。
func (c *CorporateProvider) CreatePayment(order Order) (PaymentRequest, error) {
	return PaymentRequest{}, nil
}

// HandleCallback 对公无异步回调；始终返回未支付（入账走人工审核）。
func (c *CorporateProvider) HandleCallback(req *http.Request) (ChannelReport, error) {
	return ChannelReport{}, nil
}

// QueryStatus 主动查询：以订单状态为准（由 controller 层实现），渠道层恒未支付。
func (c *CorporateProvider) QueryStatus(order Order) (ChannelReport, error) {
	return ChannelReport{}, nil
}

// Reserved 占位渠道：未签约时 Enabled=false、参数留空（§0.8 占位原则，禁止砍模块）。
// alipay_f2f / wx_native / epay 复用 / intl_* 在 M6 按本接口逐一落文件。

func init() {
	Register(&CorporateProvider{})
}

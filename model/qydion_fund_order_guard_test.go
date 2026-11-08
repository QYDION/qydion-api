package model

// 缺口②守卫回归：QYDION 网关资金订单（qydion pay 体系创建、人民币计价、
// 下单已落 QuotaSnapshot）无法在统一入账通道（service.QydionCreditOnlineOrder）
// 之外被入账——直接调用原生完成原语 RechargeEpay / Recharge 一律被拒，
// 额度与订单状态零变动。
// 管理员原生补单（ManualCompleteTopUp）的拒绝语义由既有基线
// TestManualCompleteTopUpRefusesQuotaSnapshotOrders（qydion_test.go）覆盖；
// epay 回调的正常改道由 controller/qydion_credit_convergence_test.go 覆盖。

import (
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// insertQydionFundOrderForGuardTest 造一笔人民币计价资金订单：73 元、
// 快照 5,000,000（73 / Price 7.3 × QuotaPerUnit 500000）。Amount 恒为 0——
// QYDION 下单路径（controller/qydion_pay.go:142-147）从不落 Amount。
func insertQydionFundOrderForGuardTest(t *testing.T, tradeNo string, userId int, provider string, status string) TopUp {
	t.Helper()
	topUp := TopUp{
		UserId:          userId,
		Amount:          0,
		Money:           73,
		TradeNo:         tradeNo,
		PaymentMethod:   provider,
		PaymentProvider: provider,
		CreateTime:      common.GetTimestamp(),
		Status:          status,
		QuotaSnapshot:   5_000_000,
	}
	require.NoError(t, DB.Create(&topUp).Error)
	return topUp
}

// TestRechargeEpayRejectsQydionFundOrder 伪造原生 epay 回调直达旧路径：
// 哪怕绕过 controller 分派直接调用 RechargeEpay，资金订单也必须被拒。
func TestRechargeEpayRejectsQydionFundOrder(t *testing.T) {
	truncateTables(t)

	user := insertUserForPaymentGuardTest(t, 601, 0)
	order := insertQydionFundOrderForGuardTest(t, "QYD-GUARD-ALIPAY", user.Id, "alipay_f2f", common.TopUpStatusPending)

	_, err := RechargeEpay(order.TradeNo, "alipay", "127.0.0.1")
	require.ErrorIs(t, err, ErrQydionFundOrderOutsideUnifiedChannel,
		"QYDION 资金订单必须被原生 epay 完成原语拒绝")

	assert.Equal(t, 0, getUserQuotaForPaymentGuardTest(t, user.Id), "被拒入账不得动额度")
	assert.Equal(t, common.TopUpStatusPending, getTopUpStatusForPaymentGuardTest(t, order.TradeNo), "被拒入账不得改状态")
}

// TestRechargeEpayRejectsForgedEpaySnapshotOrder 更刁钻的伪造形状：
// 直接在库里造 provider=epay + QuotaSnapshot>0 的行（渠道守卫拦不住），
// 原生完成原语仍必须按「资金订单离开统一通道」拒绝，而不是按 Amount 折算入账。
func TestRechargeEpayRejectsForgedEpaySnapshotOrder(t *testing.T) {
	truncateTables(t)

	user := insertUserForPaymentGuardTest(t, 602, 0)
	order := insertQydionFundOrderForGuardTest(t, "QYD-GUARD-FORGED-EPAY", user.Id, PaymentProviderEpay, common.TopUpStatusPending)

	_, err := RechargeEpay(order.TradeNo, "alipay", "127.0.0.1")
	require.ErrorIs(t, err, ErrQydionFundOrderOutsideUnifiedChannel,
		"provider=epay 的伪造快照行同样不得绕过统一通道")

	assert.Equal(t, 0, getUserQuotaForPaymentGuardTest(t, user.Id))
	assert.Equal(t, common.TopUpStatusPending, getTopUpStatusForPaymentGuardTest(t, order.TradeNo))
}

// TestRechargeRejectsQydionStripeFundOrder 封堵活的重复入账洞：
// QYDION stripe 订单的 checkout session ClientReferenceID 就是 trade_no
//（payment/stripe_checkout.go:92），原生 webhook 与 QYDION 回调共用同一密钥验签；
// 同一事件落到原生 webhook 时，Recharge 不得把人民币 Money 按美元折算入账
//（否则 73 元会被补成 3650 万额度，且与统一通道重复入账）。
func TestRechargeRejectsQydionStripeFundOrder(t *testing.T) {
	truncateTables(t)

	user := insertUserForPaymentGuardTest(t, 603, 0)
	order := insertQydionFundOrderForGuardTest(t, "QYD-GUARD-STRIPE", user.Id, PaymentProviderStripe, common.TopUpStatusPending)

	err := Recharge(order.TradeNo, "cus_guard", "127.0.0.1")
	require.Error(t, err, "QYDION stripe 资金订单不得经原生 stripe webhook 入账")
	// Recharge 对外收敛为通用错误（既有行为基线），守卫的可观测不变量是资金与状态零变动
	assert.Equal(t, 0, getUserQuotaForPaymentGuardTest(t, user.Id))
	assert.Equal(t, common.TopUpStatusPending, getTopUpStatusForPaymentGuardTest(t, order.TradeNo))
}

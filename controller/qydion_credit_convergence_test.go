package controller

// 缺口②收敛回归：epay 回调完成订单的分派点 settleEpayNotifyOrder。
// 1) QYDION 网关资金订单改道统一入账：快照结算（额度=QuotaSnapshot，与
//    Amount×QuotaPerUnit 无关）+ qydion_quota_transactions 流水落库；
// 2) 原生美元订单保持 RechargeEpay 原语义（额度=Amount×QuotaPerUnit，不走统一入账事务）；
// 3) 对公审核单（reviewing）绝不能被任何回调改道入账——状态机守住。
// 渠道侧全部内存替身/直调分派点，不发起任何真实外呼；不触碰 EpayNotify 的
// HTTP 层（验签与 epay 客户端构造由 payment_webhook_availability 体系另行覆盖）。

import (
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// convExpectedQuota 73 元在 Price=7.3、QuotaPerUnit=500000 下的应到账额度快照。
const convExpectedQuota = int64(5_000_000)

func setupCreditConvergenceDB(t *testing.T) *gorm.DB {
	t.Helper()
	previousDB, previousLog := model.DB, model.LOG_DB
	previousRedis, previousBatch := common.RedisEnabled, common.BatchUpdateEnabled
	previousTypes := common.MainDatabaseType()
	previousPrice, previousUnit := operation_setting.Price, common.QuotaPerUnit

	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	require.NoError(t, err)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	require.NoError(t, db.AutoMigrate(
		&model.User{}, &model.TopUp{}, &model.Log{}, &model.Option{},
		&model.QydionCoupon{}, &model.QydionCampaign{},
		&model.QydionQuotaTransaction{}, &model.QydionBonusGrant{},
	))
	model.DB, model.LOG_DB = db, db
	common.RedisEnabled, common.BatchUpdateEnabled = false, false
	common.SetDatabaseTypes(common.DatabaseTypeSQLite, common.DatabaseTypeSQLite)
	operation_setting.Price, common.QuotaPerUnit = 7.3, 500000
	service.QydionSettingsBustCache()

	t.Cleanup(func() {
		model.DB, model.LOG_DB = previousDB, previousLog
		common.RedisEnabled, common.BatchUpdateEnabled = previousRedis, previousBatch
		common.SetDatabaseTypes(previousTypes, previousTypes)
		operation_setting.Price, common.QuotaPerUnit = previousPrice, previousUnit
		service.QydionSettingsBustCache()
		_ = sqlDB.Close()
	})
	return db
}

func convSeedUserAndOrder(t *testing.T, db *gorm.DB, name, tradeNo, provider, status string, amount int64, snapshot int64) (*model.User, *model.TopUp) {
	t.Helper()
	user := model.User{Username: name, Status: common.UserStatusEnabled, AffCode: "aff-" + name}
	require.NoError(t, db.Create(&user).Error)
	order := &model.TopUp{
		UserId: user.Id, Amount: amount, Money: 73, TradeNo: tradeNo,
		PaymentProvider: provider, PaymentMethod: provider,
		CreateTime: 1761900000, Status: status, QuotaSnapshot: snapshot,
	}
	require.NoError(t, db.Create(order).Error)
	return &user, order
}

func convCountTopupLedger(t *testing.T, db *gorm.DB, tradeNo string) int64 {
	t.Helper()
	var cnt int64
	require.NoError(t, db.Model(&model.QydionQuotaTransaction{}).
		Where("trade_no = ? AND type = ?", tradeNo, model.QydionTxTypeTopup).Count(&cnt).Error)
	return cnt
}

// TestEpayNotifyDispatchRoutesQydionFundOrderToUnifiedCredit 资金订单改道统一入账：
// 额度按 QuotaSnapshot 结算、写 qydion 流水、幂等可重放。
func TestEpayNotifyDispatchRoutesQydionFundOrderToUnifiedCredit(t *testing.T) {
	db := setupCreditConvergenceDB(t)
	user, order := convSeedUserAndOrder(t, db, "conv-qydion", "QYD-CONV-1", "alipay_f2f", common.TopUpStatusPending, 0, convExpectedQuota)

	alreadyDone, err := settleEpayNotifyOrder(order.TradeNo, "alipay", "127.0.0.1")
	require.NoError(t, err)
	assert.False(t, alreadyDone)

	assert.Equal(t, convExpectedQuota, userQuota(t, db, user.Id), "额度必须按下单快照结算")
	assert.Equal(t, convExpectedQuota, int64(order.QuotaSnapshot), "快照即应到账额度")

	reloaded := model.GetTopUpByTradeNo(order.TradeNo)
	require.NotNil(t, reloaded)
	assert.Equal(t, common.TopUpStatusSuccess, reloaded.Status)

	assert.Equal(t, int64(1), convCountTopupLedger(t, db, order.TradeNo), "统一入账必须落一条 topup 资金流水")
	var ledger model.QydionQuotaTransaction
	require.NoError(t, db.Where("trade_no = ? AND type = ?", order.TradeNo, model.QydionTxTypeTopup).First(&ledger).Error)
	assert.Equal(t, convExpectedQuota, ledger.AmountQuota)
	assert.Equal(t, 73.0, ledger.Money)
	assert.Equal(t, user.Id, ledger.UserId)

	// 幂等：重复回调不改额度、不重复落流水
	alreadyDone, err = settleEpayNotifyOrder(order.TradeNo, "alipay", "127.0.0.1")
	require.NoError(t, err)
	assert.True(t, alreadyDone)
	assert.Equal(t, convExpectedQuota, userQuota(t, db, user.Id))
	assert.Equal(t, int64(1), convCountTopupLedger(t, db, order.TradeNo))
}

// TestEpayNotifyDispatchKeepsNativeEpayOrderSemantics 无快照的原生美元订单走原语义：
// 额度=Amount×QuotaPerUnit，分派器自身不触发统一入账事务
//（QydionOnTopupSettled 钩子仍由 EpayNotify 在事务外挂载，此处不越权）。
func TestEpayNotifyDispatchKeepsNativeEpayOrderSemantics(t *testing.T) {
	db := setupCreditConvergenceDB(t)
	user, order := convSeedUserAndOrder(t, db, "conv-native", "NATIVE-EPAY-CONV-1", "epay", common.TopUpStatusPending, 2, 0)

	alreadyDone, err := settleEpayNotifyOrder(order.TradeNo, "alipay", "127.0.0.1")
	require.NoError(t, err)
	assert.False(t, alreadyDone)

	assert.Equal(t, int64(2*500000), userQuota(t, db, user.Id), "原生订单语义不变：Amount×QuotaPerUnit")
	assert.Equal(t, int64(0), convCountTopupLedger(t, db, order.TradeNo), "分派器不得替原生订单走统一入账流水")

	reloaded := model.GetTopUpByTradeNo(order.TradeNo)
	require.NotNil(t, reloaded)
	assert.Equal(t, common.TopUpStatusSuccess, reloaded.Status)
	assert.Equal(t, "alipay", reloaded.PaymentMethod, "RechargeEpay 语义保持：回填实付方式")
}

// TestEpayNotifyDispatchNeverCreditsCorporateReviewingOrder 对公审核单绝不能被回调改道入账：
// 状态机只放行 pending，reviewing 单必须原样留下，等人工审核。
func TestEpayNotifyDispatchNeverCreditsCorporateReviewingOrder(t *testing.T) {
	db := setupCreditConvergenceDB(t)
	user, order := convSeedUserAndOrder(t, db, "conv-corporate", "QYD-CONV-CORP-1", "corporate", service.TopupStatusReviewing, 0, convExpectedQuota)

	_, err := settleEpayNotifyOrder(order.TradeNo, "alipay", "127.0.0.1")
	require.Error(t, err, "对公审核单不得经任何回调改道入账")

	assert.Equal(t, int64(0), userQuota(t, db, user.Id))
	assert.Equal(t, int64(0), convCountTopupLedger(t, db, order.TradeNo))

	reloaded := model.GetTopUpByTradeNo(order.TradeNo)
	require.NotNil(t, reloaded)
	assert.Equal(t, service.TopupStatusReviewing, reloaded.Status, "被拒入账不得改状态")
}

package service

// QYDION 退款业务规则回归（交接报告 §E）：
// 按快照单价折算、未消耗约束、多次部分退款累计封顶、pending 占额/failed 释放、
// 失败回补、成功落流水、并发不双扣。全部内存 SQLite 替身，不调真实渠道。

import (
	"sync"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	// refundSvcOrderCents 订单实付 100 元 = 10000 分
	refundSvcOrderCents = int64(10000)
	// refundSvcSnapshot 100 元 / Price 7.3 × QuotaPerUnit 500000 的下单时快照
	refundSvcSnapshot = int64(6849315)
)

// refundSvcPrepareTables 服务包共享内存库上补齐退款相关表（对齐 qydionPrepareBonusExpiryTables 模式）
func refundSvcPrepareTables(t *testing.T) {
	t.Helper()
	require.NoError(t, model.DB.AutoMigrate(
		&model.User{}, &model.TopUp{}, &model.Log{},
		&model.QydionRefund{}, &model.QydionQuotaTransaction{},
	))
}

// refundSvcSeed 造一个用户 + 一笔已支付的 QYDION 订单，返回 (userId, 初始余额)
func refundSvcSeed(t *testing.T, name string, quota int, provider, status string, snapshot int64) (int, int64) {
	t.Helper()
	user := model.User{Username: name, Password: "unused", Status: common.UserStatusEnabled,
		Quota: quota, AffCode: "aff-" + name}
	require.NoError(t, model.DB.Create(&user).Error)
	order := &model.TopUp{
		UserId: user.Id, Money: float64(refundSvcOrderCents) / 100, TradeNo: "RF-SVC-" + name,
		PaymentProvider: provider, PaymentMethod: provider,
		CreateTime: time.Now().Unix(), CompleteTime: time.Now().Unix(),
		Status: status, QuotaSnapshot: snapshot,
	}
	require.NoError(t, model.DB.Create(order).Error)
	return user.Id, int64(quota)
}

// refundSvcBalance 读当前余额
func refundSvcBalance(t *testing.T, userId int) int64 {
	t.Helper()
	var user model.User
	require.NoError(t, model.DB.Select("quota").Where("id = ?", userId).First(&user).Error)
	return int64(user.Quota)
}

func TestQydionRefundQuotaForCents(t *testing.T) {
	// 全额退款：折算额度恰等于快照
	q, err := QydionRefundQuotaForCents(refundSvcOrderCents, 10000, refundSvcSnapshot)
	require.NoError(t, err)
	assert.Equal(t, refundSvcSnapshot, q)

	// 半额：5000×6849315/10000 = 3424657.5 → 向下取整 3424657
	q, err = QydionRefundQuotaForCents(refundSvcOrderCents, 5000, refundSvcSnapshot)
	require.NoError(t, err)
	assert.Equal(t, int64(3424657), q)

	// 向下取整保证累计不扣穿快照：3 分 3 笔（共 9 分 < 10 分），各笔 floor 之和 ≤ 整体 floor
	var sum int64
	for range 3 {
		q, err = QydionRefundQuotaForCents(10, 3, 10)
		require.NoError(t, err)
		sum += q
	}
	assert.LessOrEqual(t, sum, int64(10-1), "累计折算额度不得把快照扣穿")

	for _, tc := range []struct {
		total, cents, snap int64
	}{
		{0, 1, 10}, {10, 0, 10}, {10, -1, 10}, {10, 11, 10}, {10, 1, 0}, {10, 1, -1},
	} {
		_, err := QydionRefundQuotaForCents(tc.total, tc.cents, tc.snap)
		require.Error(t, err, "total=%d cents=%d snap=%d 应拒绝", tc.total, tc.cents, tc.snap)
	}
}

func TestQydionPrepareRefundDeductsAndRecords(t *testing.T) {
	refundSvcPrepareTables(t)
	require.NoError(t, model.DB.Where("trade_no LIKE ?", "RF-SVC-%").Delete(&model.TopUp{}).Error)
	require.NoError(t, model.DB.Where("1=1").Delete(&model.QydionRefund{}).Error)
	userId, initial := refundSvcSeed(t, "rf-deduct", 20000000, "alipay_f2f", common.TopUpStatusSuccess, refundSvcSnapshot)

	record, err := QydionPrepareRefund("RF-SVC-rf-deduct", 5000, "admin-a")
	require.NoError(t, err)
	require.NotNil(t, record)
	assert.Equal(t, model.RefundStatusPending, record.Status)
	assert.Equal(t, int64(3424657), record.RefundQuota, "50 元按快照单价向下取整折算")
	assert.Equal(t, model.QydionRefundChannelAlipay, record.Channel)
	assert.NotEmpty(t, record.OutRequestNo)
	assert.Equal(t, "admin-a", record.Operator)

	assert.Equal(t, initial-int64(3424657), refundSvcBalance(t, userId), "预扣额度必须与折算额度一致")

	// 幂等键唯一：新退款单号不与已有重复
	other, err := QydionPrepareRefund("RF-SVC-rf-deduct", 1000, "admin-a")
	require.NoError(t, err)
	assert.NotEqual(t, record.OutRequestNo, other.OutRequestNo)
}

func TestQydionPrepareRefundRejectsInsufficientQuota(t *testing.T) {
	refundSvcPrepareTables(t)
	// 用户只剩余 300 万 < 342 万折算额度：已消耗部分不可退
	userId, initial := refundSvcSeed(t, "rf-short", 3000000, "wx_native", common.TopUpStatusSuccess, refundSvcSnapshot)

	record, err := QydionPrepareRefund("RF-SVC-rf-short", 5000, "admin-a")
	require.Error(t, err)
	assert.Nil(t, record)
	assert.Contains(t, err.Error(), "剩余额度不足", "必须透出未消耗约束语义")
	assert.Equal(t, initial, refundSvcBalance(t, userId), "被拒绝的退款不能动余额")

	var cnt int64
	require.NoError(t, model.DB.Model(&model.QydionRefund{}).Where("trade_no = ?", "RF-SVC-rf-short").Count(&cnt).Error)
	assert.Zero(t, cnt, "拒绝时不得留下退款记录")
}

func TestQydionPrepareRefundRejectsInvalidOrders(t *testing.T) {
	refundSvcPrepareTables(t)

	// 未支付订单
	refundSvcSeed(t, "rf-unpaid", 10000000, "alipay_f2f", common.TopUpStatusPending, refundSvcSnapshot)
	_, err := QydionPrepareRefund("RF-SVC-rf-unpaid", 1000, "admin-a")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "已支付")

	// 对公订单：线下人工退款，不进渠道 API
	refundSvcSeed(t, "rf-corp", 10000000, "corporate", common.TopUpStatusSuccess, refundSvcSnapshot)
	_, err = QydionPrepareRefund("RF-SVC-rf-corp", 1000, "admin-a")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "线下人工退款")

	// 非本网关在线渠道（原生 stripe）不进退款流程；退款白名单先于快照校验
	refundSvcSeed(t, "rf-legacy", 10000000, "stripe", common.TopUpStatusSuccess, 0)
	_, err = QydionPrepareRefund("RF-SVC-rf-legacy", 1000, "admin-a")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "不支持在线退款")

	// 本网关渠道但快照缺失（N07 之前的历史脏数据）：拒绝按当前单价重算折算额度
	refundSvcSeed(t, "rf-nosnap", 10000000, "alipay_f2f", common.TopUpStatusSuccess, 0)
	_, err = QydionPrepareRefund("RF-SVC-rf-nosnap", 1000, "admin-a")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "快照")

	// 订单不存在
	_, err = QydionPrepareRefund("RF-SVC-rf-nope", 1000, "admin-a")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "不存在")

	// 金额 ≤ 0
	_, err = QydionPrepareRefund("RF-SVC-rf-unpaid", 0, "admin-a")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "大于 0")
}

// TestQydionRefundPartialAccumulationCap 多次部分退款：单次 ≤ 实付−已累计，
// 折算额度累计恰等于快照，全额退完后一分也退不出去。
func TestQydionRefundPartialAccumulationCap(t *testing.T) {
	refundSvcPrepareTables(t)
	userId, initial := refundSvcSeed(t, "rf-cap", 20000000, "alipay_f2f", common.TopUpStatusSuccess, refundSvcSnapshot)
	tradeNo := "RF-SVC-rf-cap"

	// 第一笔 60 元
	first, err := QydionPrepareRefund(tradeNo, 6000, "admin-a")
	require.NoError(t, err)
	assert.Equal(t, int64(4109589), first.RefundQuota) // floor(6000×6849315/10000)
	require.NoError(t, QydionCompleteRefund(first.Id, "ALI-CRED-1", "success"))

	// 60+50 > 100：拒绝
	_, err = QydionPrepareRefund(tradeNo, 5000, "admin-a")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "可退余额")

	// 第二笔 40 元（累计正好 100 元）
	second, err := QydionPrepareRefund(tradeNo, 4000, "admin-a")
	require.NoError(t, err)
	assert.Equal(t, int64(2739726), second.RefundQuota) // floor(4000×6849315/10000)
	require.NoError(t, QydionCompleteRefund(second.Id, "ALI-CRED-2", "success"))

	// 折算额度累计恰好等于快照（floor 不漂移）
	var quotaSum int64
	require.NoError(t, model.DB.Model(&model.QydionRefund{}).
		Where("trade_no = ? AND status = ?", tradeNo, model.RefundStatusSucceeded).
		Select("COALESCE(SUM(refund_quota),0)").Scan(&quotaSum).Error)
	assert.Equal(t, refundSvcSnapshot, quotaSum)

	// 已全额退款：一分也退不出去
	_, err = QydionPrepareRefund(tradeNo, 1, "admin-a")
	require.Error(t, err)

	// 扣减总账与折算额度一致
	assert.Equal(t, initial-refundSvcSnapshot, refundSvcBalance(t, userId))
}

// TestQydionRefundPendingCountsTowardCapAndFreedOnFail pending 占用封顶空间（防并发双退），
// 渠道失败转 failed 后空间释放（钱没退出去，不应永久占住额度）。
func TestQydionRefundPendingCountsTowardCapAndFreedOnFail(t *testing.T) {
	refundSvcPrepareTables(t)
	userId, initial := refundSvcSeed(t, "rf-pendcap", 20000000, "wx_native", common.TopUpStatusSuccess, refundSvcSnapshot)
	tradeNo := "RF-SVC-rf-pendcap"

	first, err := QydionPrepareRefund(tradeNo, 10000, "admin-a") // 全额 pending
	require.NoError(t, err)

	_, err = QydionPrepareRefund(tradeNo, 1, "admin-a")
	require.Error(t, err, "pending 退款在途时必须占用封顶空间，否则并发退款会双退")

	require.NoError(t, QydionFailRefund(first.Id, "渠道拒付"))
	assert.Equal(t, initial, refundSvcBalance(t, userId), "失败回补必须回到初始余额")

	again, err := QydionPrepareRefund(tradeNo, 2000, "admin-a")
	require.NoError(t, err, "failed 不占封顶空间，可再次发起")
	require.NoError(t, QydionFailRefund(again.Id, "渠道拒付"))
}

func TestQydionFailRefundRestoresQuotaExactly(t *testing.T) {
	refundSvcPrepareTables(t)
	userId, initial := refundSvcSeed(t, "rf-fail", 20000000, "alipay_f2f", common.TopUpStatusSuccess, refundSvcSnapshot)

	record, err := QydionPrepareRefund("RF-SVC-rf-fail", 8000, "admin-b")
	require.NoError(t, err)
	require.Equal(t, initial-int64(record.RefundQuota), refundSvcBalance(t, userId))

	require.NoError(t, QydionFailRefund(record.Id, "ACQ.RAISE_AMOUNT_EXCEED"))
	assert.Equal(t, initial, refundSvcBalance(t, userId), "渠道失败后净额度必须不动")

	var stored model.QydionRefund
	require.NoError(t, model.DB.First(&stored, record.Id).Error)
	assert.Equal(t, model.RefundStatusFailed, stored.Status)
	assert.Contains(t, stored.Note, "渠道退款失败")
	assert.Contains(t, stored.Note, "ACQ.RAISE_AMOUNT_EXCEED")

	// 失败不落额度流水（净变动为 0，退款单本身就是审计记录）
	var ledgerCnt int64
	require.NoError(t, model.DB.Model(&model.QydionQuotaTransaction{}).
		Where("trade_no = ? AND type = ?", "RF-SVC-rf-fail", model.QydionTxTypeRefund).Count(&ledgerCnt).Error)
	assert.Zero(t, ledgerCnt)

	// 非 pending 单不能再定稿（幂等守卫）
	require.Error(t, QydionFailRefund(record.Id, "再次失败"))
	require.Error(t, QydionCompleteRefund(record.Id, "cred", "success"))
}

func TestQydionCompleteRefundWritesLedger(t *testing.T) {
	refundSvcPrepareTables(t)
	_, initial := refundSvcSeed(t, "rf-ok", 20000000, "wx_native", common.TopUpStatusSuccess, refundSvcSnapshot)

	record, err := QydionPrepareRefund("RF-SVC-rf-ok", 3000, "admin-c")
	require.NoError(t, err)
	require.NoError(t, QydionCompleteRefund(record.Id, "WX-REFUND-1", "SUCCESS"))

	var stored model.QydionRefund
	require.NoError(t, model.DB.First(&stored, record.Id).Error)
	assert.Equal(t, model.RefundStatusSucceeded, stored.Status)
	assert.Equal(t, "WX-REFUND-1", stored.ChannelCredential)

	var ledger model.QydionQuotaTransaction
	require.NoError(t, model.DB.Where("trade_no = ? AND type = ?", "RF-SVC-rf-ok", model.QydionTxTypeRefund).
		Order("id DESC").First(&ledger).Error)
	assert.Equal(t, -record.RefundQuota, ledger.AmountQuota, "退款流水必须为负向")
	assert.Equal(t, initial-int64(record.RefundQuota), ledger.BalanceAfter, "余额快照 = 预扣后余额")
	assert.Equal(t, "admin-c", ledger.Operator)
	assert.Equal(t, int(record.Id), ledger.RefId)

	// 重复定稿必须拒绝：渠道凭证不会被二次覆盖
	require.Error(t, QydionCompleteRefund(record.Id, "WX-REFUND-2", "SUCCESS"))

	// 不存在的退款单
	require.Error(t, QydionCompleteRefund(999999, "cred", "success"))
}

// TestQydionRefundConcurrentNoDoubleDeduct 并发退款同一订单：封顶+行锁+条件更新
// 保证恰好一笔成功，余额只扣一次。
func TestQydionRefundConcurrentNoDoubleDeduct(t *testing.T) {
	refundSvcPrepareTables(t)
	userId, initial := refundSvcSeed(t, "rf-race", 20000000, "alipay_f2f", common.TopUpStatusSuccess, refundSvcSnapshot)
	tradeNo := "RF-SVC-rf-race"

	const racers = 4
	errs := make([]error, racers)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			<-start
			_, errs[idx] = QydionPrepareRefund(tradeNo, 10000, "admin-race")
		}(i)
	}
	close(start)
	wg.Wait()

	succeeded := 0
	for _, err := range errs {
		if err == nil {
			succeeded++
		}
	}
	assert.Equal(t, 1, succeeded, "全额并发退款只能成功一笔（实付−已累计封顶）")

	var pendingCnt int64
	require.NoError(t, model.DB.Model(&model.QydionRefund{}).
		Where("trade_no = ? AND status = ?", tradeNo, model.RefundStatusPending).Count(&pendingCnt).Error)
	assert.Equal(t, int64(1), pendingCnt)

	// 预扣恰为全额快照一次，无多扣
	assert.Equal(t, initial-refundSvcSnapshot, refundSvcBalance(t, userId),
		"并发退款不得双扣额度")
}

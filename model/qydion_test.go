package model

// QYDION M4 单测：优惠券金额换算为纯逻辑；券预占状态机需内存库。

import (
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func TestQydionCouponAmount(t *testing.T) {
	// 满减：减 10 元
	amountCoupon := &QydionCoupon{Type: CouponTypeAmount, Value: 10}
	if got := QydionCouponAmount(amountCoupon, 100); got != 90 {
		t.Errorf("满减 100-10 应=90，实际 %.2f", got)
	}
	// 满减不跌破 0
	if got := QydionCouponAmount(amountCoupon, 5); got != 0 {
		t.Errorf("满减 5-10 应=0，实际 %.2f", got)
	}
	// 折扣：九折
	discountCoupon := &QydionCoupon{Type: CouponTypeDiscount, Value: 90}
	if got := QydionCouponAmount(discountCoupon, 100); got != 90 {
		t.Errorf("九折 100 应=90，实际 %.2f", got)
	}
	// nil 券原样返回
	if got := QydionCouponAmount(nil, 100); got != 100 {
		t.Errorf("nil 券应返回原价，实际 %.2f", got)
	}
}

func TestReconciliationDailyTableName(t *testing.T) {
	if (QydionReconciliationDaily{}).TableName() != "qydion_reconciliation_daily" {
		t.Errorf("对账表名不符合 §8.5c 规范")
	}
	if (QydionCoupon{}).TableName() != "qydion_coupons" {
		t.Errorf("优惠券表名错误")
	}
}

// TestQydionCouponReservation 券预占状态机。
// 回归的资金不变量：一张券只能被一笔订单占住，且已核销的券绝不会被释放回可用。
// 早先只有 ValidateCoupon（只读校验）而没有占用，同一张券可以挂到多笔订单、每笔都按
// 折后价付款，而结算只有一笔能核销成功，其余「钱收到、入账被拒」，只能手工改库。
func TestQydionCouponReservation(t *testing.T) {
	previousDB := DB
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&QydionCoupon{}))
	DB = db
	t.Cleanup(func() { DB = previousDB })

	insertCoupon := func(code string) *QydionCoupon {
		c := &QydionCoupon{Code: code, Type: CouponTypeDiscount, Value: 90,
			Status: CouponStatusUnused, ExpiresAt: time.Now().Add(24 * time.Hour)}
		require.NoError(t, db.Create(c).Error)
		return c
	}

	t.Run("预占一次成功第二次被拒", func(t *testing.T) {
		c := insertCoupon("RACE-1")
		locked, err := LockCoupon(c.Id, 11)
		require.NoError(t, err)
		require.True(t, locked, "首次预占必须成功")

		again, err := LockCoupon(c.Id, 22)
		require.NoError(t, err)
		require.False(t, again, "同一张券不能被第二笔订单占住")

		var stored QydionCoupon
		require.NoError(t, db.First(&stored, c.Id).Error)
		require.Equal(t, CouponStatusLocked, stored.Status)
		require.Equal(t, 11, stored.UsedBy, "预占必须记住占用者")
	})

	t.Run("已预占的券在校验层就被拒", func(t *testing.T) {
		c := insertCoupon("RACE-2")
		locked, err := LockCoupon(c.Id, 33)
		require.NoError(t, err)
		require.True(t, locked)

		got, msg := ValidateCoupon("RACE-2", 44, 100)
		require.Nil(t, got, "被别人预占的券不能再挂到新订单")
		require.NotEmpty(t, msg)
	})

	t.Run("关单释放可再用，核销后释放不得回退", func(t *testing.T) {
		c := insertCoupon("RACE-3")
		_, err := LockCoupon(c.Id, 55)
		require.NoError(t, err)
		require.NoError(t, ReleaseCoupon(c.Id))

		relatched, err := LockCoupon(c.Id, 66)
		require.NoError(t, err)
		require.True(t, relatched, "释放后同一张券应可再次预占")

		// 模拟结算核销：locked → used
		require.NoError(t, db.Model(&QydionCoupon{}).Where("id = ?", c.Id).
			Update("status", CouponStatusUsed).Error)
		require.NoError(t, ReleaseCoupon(c.Id))

		var stored QydionCoupon
		require.NoError(t, db.First(&stored, c.Id).Error)
		require.Equal(t, CouponStatusUsed, stored.Status, "已核销的券被释放调用打回 unused = 一张券可反复消费")
	})
}

// TestCancelPendingOrderWithCoupon covers 全量安全审计 N06：关单与放回本单的券
// 必须是一个原子动作。分两步写会留下「订单已关但券仍 locked」——那张券从此
// 谁也占不住，也谁也兑不了。
func TestCancelPendingOrderWithCoupon(t *testing.T) {
	require.NoError(t, DB.AutoMigrate(&TopUp{}, &QydionCoupon{}))
	require.NoError(t, DB.Exec("DELETE FROM top_ups").Error)
	require.NoError(t, DB.Exec("DELETE FROM qydion_coupons").Error)

	coupon := &QydionCoupon{Code: "CLOSE-1", Type: CouponTypeAmount, Value: 10,
		Status: CouponStatusLocked, UsedBy: 7, ExpiresAt: time.Now().Add(24 * time.Hour)}
	require.NoError(t, DB.Create(coupon).Error)
	order := &TopUp{UserId: 7, Money: 90, TradeNo: "QYD-CLOSE-1", PaymentProvider: "wx_native",
		Status: common.TopUpStatusPending, CouponId: coupon.Id, CreateTime: time.Now().Unix()}
	require.NoError(t, DB.Create(order).Error)

	cancelled, err := CancelPendingOrderWithCoupon(order.TradeNo, coupon.Id)
	require.NoError(t, err)
	require.True(t, cancelled)

	var storedOrder TopUp
	require.NoError(t, DB.Where("trade_no = ?", order.TradeNo).First(&storedOrder).Error)
	require.Equal(t, common.TopUpStatusCancelled, storedOrder.Status)

	var storedCoupon QydionCoupon
	require.NoError(t, DB.First(&storedCoupon, coupon.Id).Error)
	require.Equal(t, CouponStatusUnused, storedCoupon.Status)
	require.Zero(t, storedCoupon.UsedBy)

	// 重复关单不得再动券：券已经放回，再「放回」一次等于把已核销的券洗白。
	require.NoError(t, DB.Model(&storedCoupon).Update("status", CouponStatusUsed).Error)
	again, err := CancelPendingOrderWithCoupon(order.TradeNo, coupon.Id)
	require.NoError(t, err)
	require.False(t, again, "已关闭的订单不能再关一次")
	require.NoError(t, DB.First(&storedCoupon, coupon.Id).Error)
	require.Equal(t, CouponStatusUsed, storedCoupon.Status, "订单不在 pending 时绝不能回退券状态")

	// 已入账的订单同样不能被关掉，否则钱到账、状态被改回、券被放回。
	paid := &TopUp{UserId: 8, Money: 100, TradeNo: "QYD-CLOSE-2", PaymentProvider: "wx_native",
		Status: common.TopUpStatusSuccess, CouponId: coupon.Id, CreateTime: time.Now().Unix()}
	require.NoError(t, DB.Create(paid).Error)
	paidNow, err := CancelPendingOrderWithCoupon(paid.TradeNo, coupon.Id)
	require.NoError(t, err)
	require.False(t, paidNow)
	var stillPaid TopUp
	require.NoError(t, DB.Where("trade_no = ?", paid.TradeNo).First(&stillPaid).Error)
	require.Equal(t, common.TopUpStatusSuccess, stillPaid.Status)
}

// TestManualCompleteTopUpRefusesQuotaSnapshotOrders covers 全量安全审计 N07：
// 二开订单的 Money 是人民币，原生补单按美元折算，73 元会被补成 3650 万额度，
// 而且不核销券、不发满送、不写资金流水。这类单只能走渠道查证后的统一入账。
func TestManualCompleteTopUpRefusesQuotaSnapshotOrders(t *testing.T) {
	require.NoError(t, DB.AutoMigrate(&TopUp{}, &User{}, &Log{}))
	require.NoError(t, DB.Exec("DELETE FROM top_ups").Error)
	require.NoError(t, DB.Exec("DELETE FROM users").Error)

	oldUnit := common.QuotaPerUnit
	common.QuotaPerUnit = 500000
	t.Cleanup(func() { common.QuotaPerUnit = oldUnit })

	user := User{Username: "qydion-stripe-order", Password: "unused", Status: common.UserStatusEnabled, AffCode: "aff-qydion-stripe"}
	require.NoError(t, DB.Create(&user).Error)

	qydionOrder := &TopUp{UserId: user.Id, Money: 73, TradeNo: "QYD-STRIPE-1",
		PaymentProvider: "stripe", PaymentMethod: "stripe",
		Status: common.TopUpStatusPending, QuotaSnapshot: 5000000}
	require.NoError(t, DB.Create(qydionOrder).Error)

	require.Error(t, ManualCompleteTopUp(qydionOrder.TradeNo, "127.0.0.1"),
		"带额度快照的二开订单不得由原生补单处理")
	var untouched User
	require.NoError(t, DB.First(&untouched, user.Id).Error)
	require.Zero(t, untouched.Quota, "被拒绝的补单不能留下半笔入账")

	// 原生订单语义保持不变：Stripe 的 Money 就是美元。
	native := &TopUp{UserId: user.Id, Money: 10, TradeNo: "NATIVE-STRIPE-1",
		PaymentProvider: "stripe", PaymentMethod: "stripe", Status: common.TopUpStatusPending}
	require.NoError(t, DB.Create(native).Error)
	require.NoError(t, ManualCompleteTopUp(native.TradeNo, "127.0.0.1"))
	var credited User
	require.NoError(t, DB.First(&credited, user.Id).Error)
	require.Equal(t, 5000000, credited.Quota)
}

package controller

// 全量安全审计 N06 回归：pending 超 30 分钟时，「查单失败」「查到已付但入账失败」
// 「渠道未启用」都是未知，不是未付款。只有渠道确认终态才允许关单放回券。
// 渠道一律内存替身，不向微信/支付宝发起任何真实请求。

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/payment"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// expiryExpectedQuota 是 90 元在 Price=7.3、QuotaPerUnit=500000 下的应到账额度。
const expiryExpectedQuota = 6164383

var expiryChannel = struct {
	queryErr error
	paid     bool
	amount   float64
}{}

type expiryAuditProvider struct{}

func (expiryAuditProvider) Name() string        { return "expiry-audit" }
func (expiryAuditProvider) DisplayName() string { return "过期关单审计渠道" }
func (expiryAuditProvider) Enabled() bool       { return true }
func (expiryAuditProvider) CreatePayment(payment.Order) (payment.PaymentRequest, error) {
	return payment.PaymentRequest{}, errors.New("unused in expiry audit")
}
func (expiryAuditProvider) HandleCallback(*http.Request) (payment.ChannelReport, error) {
	return payment.ChannelReport{}, errors.New("unused in expiry audit")
}
func (expiryAuditProvider) QueryStatus(order payment.Order) (payment.ChannelReport, error) {
	if expiryChannel.queryErr != nil {
		return payment.ChannelReport{}, expiryChannel.queryErr
	}
	return payment.ChannelReport{OrderId: order.TradeNo, Paid: expiryChannel.paid, AmountCNY: expiryChannel.amount}, nil
}

type expiryDisabledProvider struct{}

func (expiryDisabledProvider) Name() string        { return "expiry-disabled" }
func (expiryDisabledProvider) DisplayName() string { return "未启用渠道" }
func (expiryDisabledProvider) Enabled() bool       { return false }
func (expiryDisabledProvider) CreatePayment(payment.Order) (payment.PaymentRequest, error) {
	return payment.PaymentRequest{}, nil
}
func (expiryDisabledProvider) HandleCallback(*http.Request) (payment.ChannelReport, error) {
	return payment.ChannelReport{}, nil
}
func (expiryDisabledProvider) QueryStatus(payment.Order) (payment.ChannelReport, error) {
	return payment.ChannelReport{}, errors.New("未启用渠道不应被查单")
}

func setupExpiryAuditDB(t *testing.T) *gorm.DB {
	t.Helper()
	gin.SetMode(gin.TestMode)
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
	payment.Register(expiryAuditProvider{})
	payment.Register(expiryDisabledProvider{})

	expiryChannel.queryErr, expiryChannel.paid, expiryChannel.amount = nil, false, 0
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

// staleOrder 造一笔超时 31 分钟、券已预占、尚未入账的在线订单。
func staleOrder(t *testing.T, db *gorm.DB, providerName string) (*model.User, *model.TopUp, *model.QydionCoupon) {
	t.Helper()
	user := model.User{Username: "expiry-" + providerName, Status: common.UserStatusEnabled, AffCode: "aff-" + providerName}
	require.NoError(t, db.Create(&user).Error)
	coupon := model.QydionCoupon{Code: "EXPIRY-" + providerName, Type: "amount", Value: 1,
		Status: model.CouponStatusLocked, UsedBy: user.Id, ExpiresAt: time.Now().Add(24 * time.Hour)}
	require.NoError(t, db.Create(&coupon).Error)
	order := model.TopUp{
		UserId: user.Id, Money: 90, TradeNo: "QYD-EXPIRY-" + providerName,
		PaymentProvider: providerName, PaymentMethod: providerName,
		Status: common.TopUpStatusPending, CouponId: coupon.Id,
		CreateTime: time.Now().Add(-31 * time.Minute).Unix(),
		// 90 元 / Price 7.3 × QuotaPerUnit 500000，下单时算定的快照
		QuotaSnapshot: expiryExpectedQuota,
	}
	require.NoError(t, db.Create(&order).Error)
	return &user, &order, &coupon
}

func queryOrderStatus(t *testing.T, db *gorm.DB, user *model.User, tradeNo string) string {
	t.Helper()
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Set("id", user.Id)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/api/qydion/payment/status?trade_no="+tradeNo, nil)
	QydionQueryOnlineTopup(ctx)
	reloaded := model.GetTopUpByTradeNo(tradeNo)
	require.NotNil(t, reloaded)
	return reloaded.Status
}

func TestQydionLazyExpiryKeepsOrderRetryableWhenChannelUnknown(t *testing.T) {
	db := setupExpiryAuditDB(t)
	user, order, coupon := staleOrder(t, db, "expiry-audit")

	// 渠道查询超时 = 未知
	expiryChannel.queryErr = errors.New("audit query timeout")
	status := queryOrderStatus(t, db, user, order.TradeNo)
	assert.Equal(t, common.TopUpStatusPending, status, "查单失败不能把订单关成 cancelled")

	var storedCoupon model.QydionCoupon
	require.NoError(t, db.First(&storedCoupon, coupon.Id).Error)
	assert.Equal(t, model.CouponStatusLocked, storedCoupon.Status, "订单未关就放回券，等于一张券可以抵两笔")

	// 迟到的成功通知必须仍能正常入账
	expiryChannel.queryErr = nil
	credited, err := service.QydionCreditOnlineOrder(order, "system", "迟到回调")
	require.NoError(t, err)
	assert.False(t, credited)

	var after model.User
	require.NoError(t, db.First(&after, user.Id).Error)
	assert.Equal(t, expiryExpectedQuota, after.Quota, "90 元按 Price=7.3 应到账 6,164,383 额度")

	var settled model.TopUp
	require.NoError(t, db.Where("trade_no = ?", order.TradeNo).First(&settled).Error)
	assert.Equal(t, common.TopUpStatusSuccess, settled.Status)
}

func TestQydionLazyExpiryKeepsOrderPendingWhenChannelDisabled(t *testing.T) {
	db := setupExpiryAuditDB(t)
	user, order, coupon := staleOrder(t, db, "expiry-disabled")

	status := queryOrderStatus(t, db, user, order.TradeNo)
	assert.Equal(t, common.TopUpStatusPending, status, "渠道不可用无法确认终态，不能关单")

	var storedCoupon model.QydionCoupon
	require.NoError(t, db.First(&storedCoupon, coupon.Id).Error)
	assert.Equal(t, model.CouponStatusLocked, storedCoupon.Status)
}

func TestQydionLazyExpiryClosesOnlyAfterChannelConfirmsUnpaid(t *testing.T) {
	db := setupExpiryAuditDB(t)
	user, order, coupon := staleOrder(t, db, "expiry-audit")

	expiryChannel.paid = false
	expiryChannel.amount = 0
	status := queryOrderStatus(t, db, user, order.TradeNo)
	assert.Equal(t, service.TopupStatusCancelled, status)

	var storedCoupon model.QydionCoupon
	require.NoError(t, db.First(&storedCoupon, coupon.Id).Error)
	assert.Equal(t, model.CouponStatusUnused, storedCoupon.Status, "确认未付款要放回本单的券")
	assert.Zero(t, storedCoupon.UsedBy)

	// 关掉之后仍查到已付（用户其实付了但渠道状态滞后）：走管理补入账，而不是直接改余额
	expiryChannel.paid = true
	expiryChannel.amount = 90
	reloaded := model.GetTopUpByTradeNo(order.TradeNo)
	require.NoError(t, service.QydionResettleCancelledOrder(reloaded.TradeNo, "audit", "渠道查证后补入账"))
	var after model.User
	require.NoError(t, db.First(&after, user.Id).Error)
	assert.Equal(t, expiryExpectedQuota, after.Quota)
}

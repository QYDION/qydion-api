package controller

// QYDION 退款管理 API 回归（交接报告 §E）：渠道一律内存替身（httptest + payment.Register），
// 零真实外呼。覆盖：成功退款扣额度落凭证、渠道失败记 failed 且额度不动（净额）、
// 金额校验、渠道未配置/无退款能力明确报错、超剩余额度拒绝、分页列表含订单信息、
// 悬挂 pending 单关闭回补。

import (
	"bytes"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

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

// refundExpectedQuota 100 元在 Price=7.3、QuotaPerUnit=500000 下的快照（对齐订单口径）
const refundTestSnapshot = int64(6849315)
const refundTestOrderCents = int64(10000) // 订单实付 100 元

// refundFake 渠道替身状态（同文件测试顺序执行，互斥锁仅为静态检查友好）
var (
	refundFakeMu sync.Mutex
	refundFake   struct {
		lastReq    payment.RefundRequest
		fail       bool
		failErr    string
		credential string
	}
)

type refundOkProvider struct{}

func (refundOkProvider) Name() string        { return "refund-ok" }
func (refundOkProvider) DisplayName() string { return "退款成功渠道" }
func (refundOkProvider) Enabled() bool       { return true }
func (refundOkProvider) CreatePayment(payment.Order) (payment.PaymentRequest, error) {
	return payment.PaymentRequest{}, nil
}
func (refundOkProvider) HandleCallback(*http.Request) (payment.ChannelReport, error) {
	return payment.ChannelReport{}, nil
}
func (refundOkProvider) QueryStatus(payment.Order) (payment.ChannelReport, error) {
	return payment.ChannelReport{}, nil
}
func (refundOkProvider) Refund(req payment.RefundRequest) (payment.RefundResult, error) {
	refundFakeMu.Lock()
	refundFake.lastReq = req
	refundFakeMu.Unlock()
	if refundFake.credential == "" {
		refundFake.credential = "CRED-OK-1"
	}
	return payment.RefundResult{Succeeded: true, Credential: refundFake.credential, ChannelStatus: "success"}, nil
}

type refundFailProvider struct{}

func (refundFailProvider) Name() string        { return "refund-fail" }
func (refundFailProvider) DisplayName() string { return "退款失败渠道" }
func (refundFailProvider) Enabled() bool       { return true }
func (refundFailProvider) CreatePayment(payment.Order) (payment.PaymentRequest, error) {
	return payment.PaymentRequest{}, nil
}
func (refundFailProvider) HandleCallback(*http.Request) (payment.ChannelReport, error) {
	return payment.ChannelReport{}, nil
}
func (refundFailProvider) QueryStatus(payment.Order) (payment.ChannelReport, error) {
	return payment.ChannelReport{}, nil
}
func (refundFailProvider) Refund(req payment.RefundRequest) (payment.RefundResult, error) {
	refundFakeMu.Lock()
	refundFake.lastReq = req
	refundFakeMu.Unlock()
	return payment.RefundResult{}, errors.New(refundFake.failErr)
}

type refundNoCapProvider struct{}

func (refundNoCapProvider) Name() string        { return "refund-nocap" }
func (refundNoCapProvider) DisplayName() string { return "无退款能力渠道" }
func (refundNoCapProvider) Enabled() bool       { return true }
func (refundNoCapProvider) CreatePayment(payment.Order) (payment.PaymentRequest, error) {
	return payment.PaymentRequest{}, nil
}
func (refundNoCapProvider) HandleCallback(*http.Request) (payment.ChannelReport, error) {
	return payment.ChannelReport{}, nil
}
func (refundNoCapProvider) QueryStatus(payment.Order) (payment.ChannelReport, error) {
	return payment.ChannelReport{}, nil
}

// 注意：不实现 Refund —— 用于验证 controller 对无退款能力渠道明确报错

type refundOffProvider struct{}

func (refundOffProvider) Name() string        { return "refund-off" }
func (refundOffProvider) DisplayName() string { return "未配置渠道" }
func (refundOffProvider) Enabled() bool       { return false }
func (refundOffProvider) CreatePayment(payment.Order) (payment.PaymentRequest, error) {
	return payment.PaymentRequest{}, nil
}
func (refundOffProvider) HandleCallback(*http.Request) (payment.ChannelReport, error) {
	return payment.ChannelReport{}, nil
}
func (refundOffProvider) QueryStatus(payment.Order) (payment.ChannelReport, error) {
	return payment.ChannelReport{}, nil
}
func (refundOffProvider) Refund(payment.RefundRequest) (payment.RefundResult, error) {
	return payment.RefundResult{}, nil
}

func setupRefundAuditDB(t *testing.T) *gorm.DB {
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
		&model.QydionRefund{},
	))
	model.DB, model.LOG_DB = db, db
	common.RedisEnabled, common.BatchUpdateEnabled = false, false
	common.SetDatabaseTypes(common.DatabaseTypeSQLite, common.DatabaseTypeSQLite)
	operation_setting.Price, common.QuotaPerUnit = 7.3, 500000
	service.QydionSettingsBustCache()
	payment.Register(refundOkProvider{})
	payment.Register(refundFailProvider{})
	payment.Register(refundNoCapProvider{})
	payment.Register(refundOffProvider{})
	// 测试替身渠道临时加入退款白名单（生产路径不包含；用后即删）
	service.QydionRefundChannels["refund-ok"] = model.QydionRefundChannelAlipay
	service.QydionRefundChannels["refund-fail"] = model.QydionRefundChannelWechat
	service.QydionRefundChannels["refund-nocap"] = model.QydionRefundChannelAlipay
	service.QydionRefundChannels["refund-off"] = model.QydionRefundChannelWechat

	refundFakeMu.Lock()
	refundFake.lastReq = payment.RefundRequest{}
	refundFake.fail = false
	refundFake.failErr = "渠道拒付（替身）"
	refundFake.credential = ""
	refundFakeMu.Unlock()

	t.Cleanup(func() {
		delete(service.QydionRefundChannels, "refund-ok")
		delete(service.QydionRefundChannels, "refund-fail")
		delete(service.QydionRefundChannels, "refund-nocap")
		delete(service.QydionRefundChannels, "refund-off")
		model.DB, model.LOG_DB = previousDB, previousLog
		common.RedisEnabled, common.BatchUpdateEnabled = previousRedis, previousBatch
		common.SetDatabaseTypes(previousTypes, previousTypes)
		operation_setting.Price, common.QuotaPerUnit = previousPrice, previousUnit
		service.QydionSettingsBustCache()
		_ = sqlDB.Close()
	})
	return db
}

// refundSeedOrder 造已支付 QYDION 订单，用户初始余额 initialQuota
func refundSeedOrder(t *testing.T, db *gorm.DB, name, provider string, initialQuota int) (*model.User, *model.TopUp) {
	t.Helper()
	user := model.User{Username: name, Status: common.UserStatusEnabled, Quota: initialQuota, AffCode: "aff-" + name}
	require.NoError(t, db.Create(&user).Error)
	order := &model.TopUp{
		UserId: user.Id, Money: 100, TradeNo: "RF-CT-" + name,
		PaymentProvider: provider, PaymentMethod: provider,
		CreateTime: 1761900000, CompleteTime: 1761900001,
		Status: common.TopUpStatusSuccess, QuotaSnapshot: refundTestSnapshot,
	}
	require.NoError(t, db.Create(order).Error)
	return &user, order
}

func postJSON(t *testing.T, handler gin.HandlerFunc, path string, body any) map[string]any {
	t.Helper()
	raw, err := json.Marshal(body)
	require.NoError(t, err)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Set("id", 1)
	ctx.Set("username", "admin-a")
	ctx.Request = httptest.NewRequest(http.MethodPost, path, bytes.NewReader(raw))
	ctx.Request.Header.Set("Content-Type", "application/json")
	handler(ctx)
	var out map[string]any
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &out))
	return out
}

func userQuota(t *testing.T, db *gorm.DB, userId int) int64 {
	t.Helper()
	var user model.User
	require.NoError(t, db.Select("quota").Where("id = ?", userId).First(&user).Error)
	return int64(user.Quota)
}

// TestQydionRefundCreateEndToEndWithRealAlipayProvider 端到端闭环：
// controller → 真实 AlipayF2FProvider（后台选项配置 + 网关内存替身，签名响应由测试密钥对生成）
// → 扣额度落凭证。验证真实渠道组合路径，而非只有替身渠道。
func TestQydionRefundCreateEndToEndWithRealAlipayProvider(t *testing.T) {
	db := setupRefundAuditDB(t)

	// 后台选项：真实 alipay_f2f 渠道（测试 RSA 密钥对）
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	privPEM := base64.StdEncoding.EncodeToString(x509.MarshalPKCS1PrivateKey(key))
	pubDER, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	require.NoError(t, err)
	for k, v := range map[string]string{
		"QydionAlipayAppId":      "qydion-app",
		"QydionAlipayPrivateKey": privPEM,
		"QydionAlipayPublicKey":  base64.StdEncoding.EncodeToString(pubDER),
	} {
		require.NoError(t, db.Create(&model.Option{Key: k, Value: v}).Error)
	}
	service.QydionSettingsBustCache()

	// 网关替身：受理成功，返回已签名的 alipay.trade.refund 响应
	node := `{"code":"10000","msg":"Success","out_trade_no":"RF-CT-e2e","trade_no":"2026110822001999","fund_change":"Y"}`
	digest := sha256.Sum256([]byte(node))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	require.NoError(t, err)
	signedBody := `{"alipay_trade_refund_response":` + node +
		`,"sign":"` + base64.StdEncoding.EncodeToString(sig) + `"}`
	var sawMethod string
	previousTransport := http.DefaultTransport
	http.DefaultTransport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if err := r.ParseForm(); err == nil {
			sawMethod = r.PostForm.Get("method")
		}
		return &http.Response{
			StatusCode: http.StatusOK, Header: make(http.Header),
			Body: io.NopCloser(strings.NewReader(signedBody)),
		}, nil
	})
	t.Cleanup(func() { http.DefaultTransport = previousTransport })

	_, order := refundSeedOrder(t, db, "e2e", "alipay_f2f", 20000000)
	out := postJSON(t, QydionCreateRefund, "/api/qydion/refund/create",
		map[string]any{"trade_no": order.TradeNo, "refund_money": 50})
	require.True(t, out["success"].(bool), "响应：%v", out)
	assert.Equal(t, "alipay.trade.refund", sawMethod, "必须走真实渠道的退款接口")

	var record model.QydionRefund
	require.NoError(t, db.Where("trade_no = ?", order.TradeNo).First(&record).Error)
	assert.Equal(t, model.RefundStatusSucceeded, record.Status)
	assert.Equal(t, "2026110822001999", record.ChannelCredential, "凭证取支付宝交易号")
	// 50 元 = 5000 分 → floor(5000×6849315/10000) = 3424657
	assert.Equal(t, int64(3424657), record.RefundQuota)
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestQydionRefundCreateSuccessDeductsAndRecords(t *testing.T) {
	db := setupRefundAuditDB(t)
	user, order := refundSeedOrder(t, db, "ok", "refund-ok", 20000000)

	out := postJSON(t, QydionCreateRefund, "/api/qydion/refund/create",
		map[string]any{"trade_no": order.TradeNo, "refund_money": 30, "reason": "客户申请"})
	require.True(t, out["success"].(bool), "响应：%v", out)

	// 30 元 = 3000 分 → floor(3000×6849315/10000) = 2054794
	expected := int64(2054794)
	assert.Equal(t, 20000000-expected, userQuota(t, db, user.Id))

	var record model.QydionRefund
	require.NoError(t, db.Where("trade_no = ?", order.TradeNo).First(&record).Error)
	assert.Equal(t, model.RefundStatusSucceeded, record.Status)
	assert.Equal(t, expected, record.RefundQuota)
	assert.Equal(t, int64(3000), record.RefundCents)
	assert.Equal(t, "CRED-OK-1", record.ChannelCredential)
	assert.Equal(t, model.QydionRefundChannelAlipay, record.Channel)
	assert.Equal(t, "admin-a", record.Operator)

	// 成功落负向流水
	var ledger model.QydionQuotaTransaction
	require.NoError(t, db.Where("trade_no = ? AND type = ?", order.TradeNo, model.QydionTxTypeRefund).First(&ledger).Error)
	assert.Equal(t, -expected, ledger.AmountQuota)

	// 渠道替身收到的请求字段正确
	refundFakeMu.Lock()
	defer refundFakeMu.Unlock()
	assert.Equal(t, order.TradeNo, refundFake.lastReq.TradeNo)
	assert.Equal(t, record.OutRequestNo, refundFake.lastReq.OutRequestNo)
	assert.Equal(t, int64(3000), refundFake.lastReq.RefundCents)
	assert.Equal(t, refundTestOrderCents, refundFake.lastReq.TotalCents)
}

func TestQydionRefundCreateChannelFailureRecordsFailedAndRestores(t *testing.T) {
	db := setupRefundAuditDB(t)
	user, order := refundSeedOrder(t, db, "fail", "refund-fail", 20000000)

	out := postJSON(t, QydionCreateRefund, "/api/qydion/refund/create",
		map[string]any{"trade_no": order.TradeNo, "refund_money": 10})
	require.False(t, out["success"].(bool), "渠道失败必须向管理员报错")
	assert.Contains(t, out["message"], "渠道拒付")

	// 渠道失败：记录 failed 且额度净不动
	var record model.QydionRefund
	require.NoError(t, db.Where("trade_no = ?", order.TradeNo).First(&record).Error)
	assert.Equal(t, model.RefundStatusFailed, record.Status)
	assert.Equal(t, int64(1000), record.RefundCents)
	assert.Equal(t, 20000000, int(userQuota(t, db, user.Id)), "失败后净额度必须回到初始值")

	var ledgerCnt int64
	require.NoError(t, db.Model(&model.QydionQuotaTransaction{}).
		Where("trade_no = ? AND type = ?", order.TradeNo, model.QydionTxTypeRefund).Count(&ledgerCnt).Error)
	assert.Zero(t, ledgerCnt, "失败不落退款流水")
}

func TestQydionRefundCreateMoneyValidation(t *testing.T) {
	db := setupRefundAuditDB(t)
	user, order := refundSeedOrder(t, db, "money", "refund-ok", 20000000)

	cases := []struct {
		money  float64
		reason string
	}{
		{0, "大于 0"},
		{-5, "大于 0"},
		{10.555, "两位小数"},
		{0.001, "两位小数"},
	}
	for _, tc := range cases {
		out := postJSON(t, QydionCreateRefund, "/api/qydion/refund/create",
			map[string]any{"trade_no": order.TradeNo, "refund_money": tc.money})
		require.False(t, out["success"].(bool), "金额 %.3f 应被拒绝", tc.money)
		assert.Contains(t, out["message"], tc.reason)
	}
	assert.Equal(t, int64(20000000), userQuota(t, db, user.Id), "被拒请求不得动余额")

	var cnt int64
	require.NoError(t, db.Model(&model.QydionRefund{}).Where("trade_no = ?", order.TradeNo).Count(&cnt).Error)
	assert.Zero(t, cnt)

	// 缺 trade_no
	out := postJSON(t, QydionCreateRefund, "/api/qydion/refund/create", map[string]any{"refund_money": 10})
	assert.False(t, out["success"].(bool))
}

func TestQydionRefundCreateChannelNotConfiguredOrNoCapability(t *testing.T) {
	db := setupRefundAuditDB(t)
	offUser, offOrder := refundSeedOrder(t, db, "off", "refund-off", 20000000)
	out := postJSON(t, QydionCreateRefund, "/api/qydion/refund/create",
		map[string]any{"trade_no": offOrder.TradeNo, "refund_money": 10})
	require.False(t, out["success"].(bool))
	assert.Contains(t, out["message"], "未配置或未启用")
	assert.Equal(t, int64(20000000), userQuota(t, db, offUser.Id), "渠道不可用必须回补预扣额度")
	var offRecord model.QydionRefund
	require.NoError(t, db.Where("trade_no = ?", offOrder.TradeNo).First(&offRecord).Error)
	assert.Equal(t, model.RefundStatusFailed, offRecord.Status)

	noCapUser, noCapOrder := refundSeedOrder(t, db, "nocap", "refund-nocap", 20000000)
	out = postJSON(t, QydionCreateRefund, "/api/qydion/refund/create",
		map[string]any{"trade_no": noCapOrder.TradeNo, "refund_money": 10})
	require.False(t, out["success"].(bool))
	assert.Contains(t, out["message"], "不支持在线退款")
	assert.Equal(t, int64(20000000), userQuota(t, db, noCapUser.Id))
}

func TestQydionRefundCreateRejectsWhenQuotaInsufficient(t *testing.T) {
	db := setupRefundAuditDB(t)
	// 用户余额 200 万 < 30 元折算额度 205.48 万：已消耗部分不可退
	user, order := refundSeedOrder(t, db, "short", "refund-ok", 2000000)

	out := postJSON(t, QydionCreateRefund, "/api/qydion/refund/create",
		map[string]any{"trade_no": order.TradeNo, "refund_money": 30})
	require.False(t, out["success"].(bool))
	assert.Contains(t, out["message"], "剩余额度不足")
	assert.Equal(t, int64(2000000), userQuota(t, db, user.Id), "拒绝时不得动余额")
}

func TestQydionRefundListPaginationWithOrderInfo(t *testing.T) {
	db := setupRefundAuditDB(t)
	_, order := refundSeedOrder(t, db, "list", "refund-ok", 20000000)
	// 两笔部分退款（30 元 + 20 元），累计 50 元
	out := postJSON(t, QydionCreateRefund, "/api/qydion/refund/create",
		map[string]any{"trade_no": order.TradeNo, "refund_money": 30})
	require.True(t, out["success"].(bool), "%v", out)
	out = postJSON(t, QydionCreateRefund, "/api/qydion/refund/create",
		map[string]any{"trade_no": order.TradeNo, "refund_money": 20})
	require.True(t, out["success"].(bool), "%v", out)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Set("id", 1)
	ctx.Set("username", "admin-a")
	ctx.Request = httptest.NewRequest(http.MethodGet, "/api/qydion/refund/list?p=1&page_size=1&trade_no="+order.TradeNo, nil)
	QydionListRefunds(ctx)
	var listOut struct {
		Success bool `json:"success"`
		Data    struct {
			Items []struct {
				TradeNo       string  `json:"trade_no"`
				Status        string  `json:"status"`
				RefundCents   int64   `json:"refund_cents"`
				Username      string  `json:"username"`
				OrderMoney    float64 `json:"order_money"`
				OrderStatus   string  `json:"order_status"`
				QuotaSnapshot int64   `json:"quota_snapshot"`
			} `json:"items"`
			Total    int64 `json:"total"`
			Page     int   `json:"page"`
			PageSize int   `json:"page_size"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &listOut))
	require.True(t, listOut.Success)
	assert.Equal(t, int64(2), listOut.Data.Total)
	assert.Equal(t, 1, listOut.Data.Page)
	require.Len(t, listOut.Data.Items, 1)
	// 倒序：最新的一笔（20 元）在前
	assert.Equal(t, int64(2000), listOut.Data.Items[0].RefundCents)
	assert.Equal(t, "list", listOut.Data.Items[0].Username, "列表必须含用户名")
	assert.Equal(t, float64(100), listOut.Data.Items[0].OrderMoney, "列表必须含订单实付")
	assert.Equal(t, common.TopUpStatusSuccess, listOut.Data.Items[0].OrderStatus)
	assert.Equal(t, refundTestSnapshot, listOut.Data.Items[0].QuotaSnapshot)
}

func TestQydionClosePendingRefundRestoresQuota(t *testing.T) {
	db := setupRefundAuditDB(t)
	user, order := refundSeedOrder(t, db, "close", "refund-ok", 20000000)

	// 直接经 service 制造悬挂 pending（模拟进程在事务A/B之间中断）
	record, err := service.QydionPrepareRefund(order.TradeNo, 5000, "admin-a")
	require.NoError(t, err)
	require.Equal(t, 20000000-int64(3424657), userQuota(t, db, user.Id))

	out := postJSON(t, QydionCloseRefund, "/api/qydion/refund/close", map[string]any{"refund_id": record.Id})
	require.True(t, out["success"].(bool), "%v", out)
	assert.Equal(t, int64(20000000), userQuota(t, db, user.Id), "关闭悬挂单必须原额回补")

	var stored model.QydionRefund
	require.NoError(t, db.First(&stored, record.Id).Error)
	assert.Equal(t, model.RefundStatusFailed, stored.Status)

	// 已定稿的单不能再关
	out = postJSON(t, QydionCloseRefund, "/api/qydion/refund/close", map[string]any{"refund_id": record.Id})
	assert.False(t, out["success"].(bool))

	// 用户侧：本人退款记录可见（含刚被关闭的这笔）
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Set("id", user.Id)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/api/qydion/refund/my", nil)
	QydionListMyRefunds(ctx)
	var myOut struct {
		Success bool `json:"success"`
		Data    []struct {
			Id     int64  `json:"id"`
			Status string `json:"status"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &myOut))
	require.True(t, myOut.Success)
	require.Len(t, myOut.Data, 1)
	assert.Equal(t, record.Id, myOut.Data[0].Id)
}

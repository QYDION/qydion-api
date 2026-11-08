package controller

// QYDION 在线支付（M7 §8.6b 全量）：用户下单/查单 + 渠道异步回调（公开验签）+ 后台支付设置。
// 渠道：alipay_f2f / wx_native / stripe（填参即启用）；corporate 走 qydion_topup.go 人工流。

import (
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/payment"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting/system_setting"
	"github.com/gin-gonic/gin"
)

// qydionAmountTolerance 渠道回执金额与订单应付的容差（人民币元）。
// 币种折算到最小单位再回折必然产生分位取整差，0.05 与折到分后的最大误差同量级。
const qydionAmountTolerance = 0.05

// ---------- 用户侧 ----------

// QydionListPaymentMethods 用户侧：可用充值渠道（名称/展示/交互类型）
func QydionListPaymentMethods(c *gin.Context) {
	providers := payment.ListAll()
	type method struct {
		Name        string `json:"name"`
		DisplayName string `json:"display_name"`
		Enabled     bool   `json:"enabled"`
		Mode        string `json:"mode"` // qr=扫码 / redirect=跳转 / manual=人工（对公）
	}
	result := make([]method, 0, len(providers))
	for _, p := range providers {
		if p.Name() == "epay" || strings.HasPrefix(p.Name(), "intl_") {
			continue // 预留渠道不展示
		}
		m := method{Name: p.Name(), DisplayName: p.DisplayName(), Enabled: p.Enabled()}
		switch p.Name() {
		case "alipay_f2f", "wx_native":
			m.Mode = "qr"
		case "stripe":
			m.Mode = "redirect"
		case "corporate":
			m.Mode = "manual"
		default:
			m.Mode = "qr"
		}
		result = append(result, m)
	}
	common.ApiSuccess(c, result)
}

// qydionPayCreateLimiter 每用户下单频控（10/分钟，防刷单占库存号段）
var qydionPayCreateLimiter syncMapOfInt64

// QydionCreateOnlineTopup 用户侧：创建在线充值订单（pending）→ 渠道下单 → 返回二维码/跳转
// body: { "provider": "alipay_f2f|wx_native|stripe", "money": 100, "coupon_code": "可选" }
func QydionCreateOnlineTopup(c *gin.Context) {
	userId := c.GetInt("id")
	var req struct {
		Provider   string  `json:"provider"`
		Money      float64 `json:"money"`
		CouponCode string  `json:"coupon_code"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || req.Provider == "" {
		common.ApiErrorMsg(c, "参数无效")
		return
	}
	if !qydionPayCreateLimiter.allow(fmt.Sprintf("paycreate:%d", userId), 10, time.Minute) {
		common.ApiErrorMsg(c, "操作过于频繁，请稍后再试")
		return
	}
	provider := payment.Get(req.Provider)
	if provider == nil || !provider.Enabled() {
		common.ApiErrorMsg(c, "该支付渠道未开放（未配置或未启用）")
		return
	}
	if req.Money < 1 || req.Money > 1000000 || req.Money != qydionRoundMoney(req.Money) {
		common.ApiErrorMsg(c, "充值金额必须为 1~1000000 之间的两位小数")
		return
	}
	// 优惠券（可选）：校验 → 预占 → 折减
	// 预占必须在下单时做，不能只校验：一张券挂两笔订单、两笔都付款时，
	// 后一笔会在结算核销处命中 0 行而被拒绝入账，用户钱已付、额度永不到账。
	payMoney := req.Money
	var coupon *model.QydionCoupon
	if code := strings.TrimSpace(req.CouponCode); code != "" {
		var msg string
		coupon, msg = model.ValidateCoupon(code, userId, req.Money)
		if coupon == nil {
			common.ApiErrorMsg(c, msg)
			return
		}
		locked, err := model.LockCoupon(coupon.Id, userId)
		if err != nil {
			common.SysError(common.LogText("QYDION 券预占失败 coupon=%d: %v", coupon.Id, err))
			common.ApiErrorMsg(c, "优惠码占用失败，请稍后重试")
			return
		}
		if !locked {
			common.ApiErrorMsg(c, "该优惠码刚被其他订单占用，请重新下单或更换优惠码")
			return
		}
		payMoney = qydionRoundMoney(model.QydionCouponAmount(coupon, req.Money))
		if payMoney <= 0 {
			_ = model.ReleaseCoupon(coupon.Id)
			common.ApiErrorMsg(c, "优惠券已抵扣全部金额，请直接使用余额或调整金额")
			return
		}
	}
	// 下单频次（同对公：每日 ≤20）
	var todayCnt int64
	dayStart := time.Now().Truncate(24 * time.Hour).Unix()
	if err := model.DB.Model(&model.TopUp{}).
		Where("user_id = ? AND payment_provider = ? AND create_time >= ?", userId, req.Provider, dayStart).
		Count(&todayCnt).Error; err == nil && todayCnt >= 20 {
		releaseQydionCoupon(coupon)
		common.ApiErrorMsg(c, "今日该渠道下单次数已达上限（20 次/天）")
		return
	}
	baseURL, err := qydionSiteBaseURL()
	if err != nil {
		releaseQydionCoupon(coupon)
		common.ApiErrorMsg(c, err.Error())
		return
	}
	tradeNo := fmt.Sprintf("QYD%d%06d", time.Now().Unix(), qydionSecureInt(1000000))
	// 下单即锁定应到账额度：入账/查单/补单都按这个快照，后台中途改单价不影响已下单。
	snapshot, err := service.QydionQuotaForMoney(payMoney)
	if err != nil {
		releaseQydionCoupon(coupon)
		common.SysError(common.LogText("QYDION 额度快照计算失败 money=%.2f: %v", payMoney, err))
		common.ApiErrorMsg(c, "充值额度计算失败，请联系管理员")
		return
	}
	topUp := &model.TopUp{
		UserId: userId, Money: payMoney, TradeNo: tradeNo,
		PaymentMethod: req.Provider, PaymentProvider: req.Provider,
		CreateTime: time.Now().Unix(), Status: common.TopUpStatusPending,
		QuotaSnapshot: snapshot,
	}
	if coupon != nil {
		topUp.CouponId = coupon.Id
	}
	if err := topUp.Insert(); err != nil {
		releaseQydionCoupon(coupon)
		common.SysError(common.LogText("QYDION 在线订单创建失败: %v", err))
		common.ApiErrorMsg(c, "订单创建失败，请稍后重试")
		return
	}
	pay, err := provider.CreatePayment(payment.Order{
		TradeNo: tradeNo, UserId: userId, Money: payMoney,
		Subject:   "QYDION API 充值 " + fmt.Sprintf("%.2f", payMoney) + " 元",
		NotifyURL: baseURL + "/api/qydion/payment/notify/" + req.Provider,
		ReturnURL: baseURL + "/console",
	})
	if err != nil {
		// 渠道下单失败：原子关单并放回本单的券，避免 pending 挂单与券被空占
		if _, cancelErr := model.CancelPendingOrderWithCoupon(tradeNo, topUp.CouponId); cancelErr != nil {
			common.SysError(common.LogText("QYDION 下单失败后的关单失败 trade_no=%s: %v", tradeNo, cancelErr))
		}
		common.SysError(common.LogText("QYDION 渠道下单失败 provider=%s: %v", req.Provider, err))
		common.ApiErrorMsg(c, "发起支付失败："+err.Error())
		return
	}
	// 渠道侧引用落库（Stripe checkout session id 等），供查单兜底定位会话
	if pay.ProviderRef != "" {
		if err := model.DB.Model(&model.TopUp{}).Where("trade_no = ?", tradeNo).
			Update("provider_ref", pay.ProviderRef).Error; err != nil {
			common.SysError(common.LogText("QYDION 渠道引用落库失败 trade_no=%s: %v", tradeNo, err))
		}
	}
	common.ApiSuccess(c, gin.H{
		"trade_no": tradeNo, "money": payMoney, "status": topUp.Status,
		"pay_url": pay.PayUrl, "qr_content": pay.QrContent,
		"expires_in": 1800,
	})
}

// QydionQueryOnlineTopup 用户侧：订单状态（pending 时主动向渠道查单兜底，paid 即入账）
func QydionQueryOnlineTopup(c *gin.Context) {
	userId := c.GetInt("id")
	tradeNo := strings.TrimSpace(c.Query("trade_no"))
	if tradeNo == "" {
		common.ApiErrorMsg(c, "缺少 trade_no")
		return
	}
	topUp := model.GetTopUpByTradeNo(tradeNo)
	if topUp == nil || topUp.UserId != userId {
		common.ApiErrorMsg(c, "订单不存在")
		return
	}
	provider := payment.Get(topUp.PaymentProvider)
	providerUsable := provider != nil && provider.Enabled()

	// 懒过期：pending 超 30 分钟，先向渠道确认终态，确认未付款才关单。
	// 「查不到」「查失败」「查到已付但入账失败」都不是「未付款」——
	// 早期实现在这三种情况下照样关单并放回券，后到的付款通知就再也入不了账。
	if topUp.Status == common.TopUpStatusPending && time.Now().Unix()-topUp.CreateTime > 1800 {
		if !providerUsable {
			common.SysError(common.LogText("QYDION 超时单无法向渠道确认，保持 pending trade_no=%s provider=%s", tradeNo, topUp.PaymentProvider))
			common.ApiSuccess(c, gin.H{"trade_no": tradeNo, "status": common.TopUpStatusPending, "awaiting_channel": true})
			return
		}
		report, err := provider.QueryStatus(payment.Order{
			TradeNo: topUp.TradeNo, UserId: topUp.UserId, Money: topUp.Money,
			ProviderRef: topUp.ProviderRef,
		})
		if err != nil {
			common.SysError(common.LogText("QYDION 超时单渠道查证失败，保持 pending trade_no=%s: %v", tradeNo, err))
			common.ApiSuccess(c, gin.H{"trade_no": tradeNo, "status": common.TopUpStatusPending, "awaiting_channel": true})
			return
		}
		if report.Paid {
			if err := qydionSettleReported(provider, topUp, report, "查单兜底（超时前）"); err == nil {
				common.ApiSuccess(c, gin.H{"trade_no": tradeNo, "status": common.TopUpStatusSuccess, "money": topUp.Money})
				return
			} else {
				// 渠道已确认收到钱：入账失败只能重试或人工介入，不能关单把订单拒掉。
				common.SysError(common.LogText("QYDION 超时前查单入账失败，保持 pending trade_no=%s: %v", tradeNo, err))
				common.ApiSuccess(c, gin.H{"trade_no": tradeNo, "status": common.TopUpStatusPending, "awaiting_channel": true})
				return
			}
		}
		// 渠道确认未付款，且渠道侧的 time_expire/timeout_express 都是 30 分钟：
		// 这是终态，可以关单并放回本单预占的券（同一事务，不留半态）。
		cancelled, err := model.CancelPendingOrderWithCoupon(tradeNo, topUp.CouponId)
		if err != nil {
			common.SysError(common.LogText("QYDION 关单失败 trade_no=%s: %v", tradeNo, err))
			common.ApiErrorMsg(c, "订单状态更新失败，请稍后重试")
			return
		}
		status := topUp.Status
		if cancelled {
			status = service.TopupStatusCancelled
		} else {
			// 并发下已被回调或其他操作处理，按最新状态回复，绝不覆盖已 success 的账
			if fresh := model.GetTopUpByTradeNo(tradeNo); fresh != nil {
				status = fresh.Status
			}
		}
		common.ApiSuccess(c, gin.H{"trade_no": tradeNo, "status": status, "expired": cancelled})
		return
	}
	if topUp.Status == common.TopUpStatusPending && providerUsable {
		if report, err := provider.QueryStatus(payment.Order{
			TradeNo: topUp.TradeNo, UserId: topUp.UserId, Money: topUp.Money,
			ProviderRef: topUp.ProviderRef,
		}); err == nil && report.Paid {
			if err := qydionSettleReported(provider, topUp, report, "查单兜底"); err == nil {
				topUp.Status = common.TopUpStatusSuccess
			} else {
				common.SysError(common.LogText("QYDION 查单入账失败 trade_no=%s: %v", tradeNo, err))
			}
		}
	}
	common.ApiSuccess(c, gin.H{"trade_no": tradeNo, "status": topUp.Status, "money": topUp.Money})
}

// ---------- 渠道异步回调（公开路由；安全完全依赖渠道层验签） ----------

// QydionPaymentNotify 渠道回调统一入口：验签 → 金额比对 → 幂等入账 → 按渠道规范应答
func QydionPaymentNotify(c *gin.Context) {
	providerName := c.Param("provider")
	provider := payment.Get(providerName)
	if provider == nil || !provider.Enabled() {
		common.SysError(common.LogText("QYDION 回调被拒：渠道未注册或未启用 provider=%s", providerName))
		c.String(http.StatusNotFound, "provider not enabled")
		return
	}
	report, err := provider.HandleCallback(c.Request)
	if err != nil {
		common.SysError(common.LogText("QYDION 回调验签失败 provider=%s: %v", providerName, err))
		qydionNotifyAck(c, providerName, false)
		return
	}
	if report.OrderId == "" || !report.Paid {
		// 未支付事件（如微信非 SUCCESS 状态）：确认收到但不入账
		common.SysLog(common.LogText("QYDION 回调非支付完成事件 provider=%s trade_no=%s", providerName, report.OrderId))
		qydionNotifyAck(c, providerName, true)
		return
	}
	topUp := model.GetTopUpByTradeNo(report.OrderId)
	if topUp == nil {
		common.SysError(common.LogText("QYDION 回调订单不存在 trade_no=%s provider=%s", report.OrderId, providerName))
		qydionNotifyAck(c, providerName, false)
		return
	}
	if topUp.Status == common.TopUpStatusSuccess {
		qydionNotifyAck(c, providerName, true) // 幂等：重复回调直接确认
		return
	}
	if err := qydionSettleReported(provider, topUp, report, "异步回调"); err != nil {
		common.SysError(common.LogText("QYDION 在线入账失败 trade_no=%s: %v", report.OrderId, err))
		qydionNotifyAck(c, providerName, false) // 不确认 → 渠道重试（配合幂等可安全重放）
		return
	}
	model.RecordLog(topUp.UserId, model.LogTypeTopup, common.NewMessage("在线充值成功，实付 {{money}} 元已到账", map[string]any{"money": fmt.Sprintf("%.2f", topUp.Money)}))
	qydionNotifyAck(c, providerName, true)
}

// ---------- 入账共用路径 ----------

// qydionSettleReported 校验渠道回执金额后入账，回调与查单两条路径共用同一套判定。
// 金额一律取自本次 ChannelReport（随返回值交出），不再从渠道单例字段回读——
// 那是跨请求共享状态，并发回调下会把 A 单的金额比到 B 单上。
func qydionSettleReported(provider payment.Provider, topUp *model.TopUp, report payment.ChannelReport, via string) error {
	// 渠道报不出金额 = 无法证明实付额，fail-closed 拒入账，等渠道重试或人工介入
	if report.AmountCNY <= 0 {
		return fmt.Errorf("渠道未报告实收金额，拒绝按折后价入账 trade_no=%s provider=%s", topUp.TradeNo, topUp.PaymentProvider)
	}
	// 容差 ±0.05 元：币种折算到最小单位再回折必然有分位取整差
	if math.Abs(report.AmountCNY-topUp.Money) > qydionAmountTolerance {
		return fmt.Errorf("回调金额不符 trade_no=%s order=%.2f reported=%.2f provider=%s（疑似篡改或配置错位，已拒绝入账）",
			topUp.TradeNo, topUp.Money, report.AmountCNY, topUp.PaymentProvider)
	}
	if _, err := service.QydionCreditOnlineOrder(topUp, "system", "在线支付到账（"+provider.DisplayName()+"·"+via+"）"); err != nil {
		return err
	}
	return nil
}

// releaseQydionCoupon 释放本单预占的券（locked → unused）
func releaseQydionCoupon(coupon *model.QydionCoupon) {
	if coupon != nil {
		releaseQydionCouponId(coupon.Id)
	}
}

func releaseQydionCouponId(couponId int) {
	if couponId <= 0 {
		return
	}
	if err := model.ReleaseCoupon(couponId); err != nil {
		common.SysError(common.LogText("QYDION 券预占释放失败 coupon=%d: %v", couponId, err))
	}
}

// qydionSiteBaseURL 回调/跳转地址的唯一来源是后台「站点地址」。
// 早期实现用 c.Request.Host 拼接，等于把 NotifyURL 交给请求方决定：
// 用自己的 Host 下单，渠道回调与订单信息就会被引到攻击者的域。
func qydionSiteBaseURL() (string, error) {
	const unconfigured = "未配置在线支付所需的站点地址：请在后台「系统设置 → 站点地址」填写本站公网 HTTPS 地址（如 https://api.qydion.cn）"
	addr := strings.TrimSpace(system_setting.ServerAddress)
	if addr == "" || addr == "http://localhost:3000" {
		return "", errors.New(unconfigured)
	}
	u, err := url.Parse(addr)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return "", errors.New(unconfigured)
	}
	if h := strings.ToLower(u.Hostname()); h == "localhost" || h == "127.0.0.1" || h == "::1" {
		return "", errors.New(unconfigured)
	}
	return strings.TrimRight(addr, "/"), nil
}

// qydionRoundMoney 人民币归一到分（两位小数）。
// 券折减是浮点乘法，不归一就会带着 0.003333 这类尾数进订单，
// 再被 ±0.05 元入账容差吞掉，形成每单「入账 > 实收」的漂移。
func qydionRoundMoney(v float64) float64 {
	return math.Round(v*100) / 100
}

// QydionForceSettleCancelledTopup 管理侧：凭渠道查证给已关单订单补入账（§8.6a 应急口）。
// 入账依据只有渠道侧的签名查询结果，不看请求里传了什么；查不到已支付就坚决拒绝。
// 没有这个口时，「回调丢失 + 订单被超时关单」只能靠手工改库救，用户付了钱拿不到额度。
func QydionForceSettleCancelledTopup(c *gin.Context) {
	var req struct {
		TradeNo string `json:"trade_no"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || strings.TrimSpace(req.TradeNo) == "" {
		common.ApiErrorMsg(c, "缺少 trade_no")
		return
	}
	tradeNo := strings.TrimSpace(req.TradeNo)
	topUp := model.GetTopUpByTradeNo(tradeNo)
	if topUp == nil {
		common.ApiErrorMsg(c, "订单不存在")
		return
	}
	// 只对 QYDION 在线渠道开放：对公走人工审核流，其他渠道的单不归这个口
	switch topUp.PaymentProvider {
	case "alipay_f2f", "wx_native", "stripe":
	default:
		common.ApiErrorMsg(c, "该订单渠道不支持补入账（对公订单请走审核流程）")
		return
	}
	if topUp.Status == common.TopUpStatusSuccess {
		common.ApiSuccess(c, gin.H{"trade_no": tradeNo, "status": topUp.Status, "already_done": true})
		return
	}
	provider := payment.Get(topUp.PaymentProvider)
	if provider == nil || !provider.Enabled() {
		common.ApiErrorMsg(c, "渠道未启用，无法向渠道查证，拒绝补入账")
		return
	}
	report, err := provider.QueryStatus(payment.Order{
		TradeNo: topUp.TradeNo, UserId: topUp.UserId, Money: topUp.Money,
		ProviderRef: topUp.ProviderRef,
	})
	if err != nil {
		common.SysError(common.LogText("QYDION 补入账查证失败 trade_no=%s: %v", tradeNo, err))
		common.ApiErrorMsg(c, "渠道查证失败，请稍后重试")
		return
	}
	if !report.Paid {
		common.SysError(common.LogText("QYDION 补入账被拒：渠道确认未支付 trade_no=%s provider=%s operator=%s",
			tradeNo, topUp.PaymentProvider, c.GetString("username")))
		common.ApiErrorMsg(c, "渠道确认该单未支付，不能补入账")
		return
	}
	if report.AmountCNY <= 0 || math.Abs(report.AmountCNY-topUp.Money) > qydionAmountTolerance {
		common.SysError(common.LogText("QYDION 补入账被拒：渠道金额不符 trade_no=%s order=%.2f reported=%.2f",
			tradeNo, topUp.Money, report.AmountCNY))
		common.ApiErrorMsg(c, "渠道回执金额与订单不符，需人工核对")
		return
	}
	operator := c.GetString("username")
	if operator == "" {
		operator = fmt.Sprintf("admin#%d", c.GetInt("id"))
	}
	if topUp.Status == common.TopUpStatusPending {
		// 仍挂着 pending：直接走查单入账
		if err := qydionSettleReported(provider, topUp, report, "管理补入账"); err != nil {
			common.SysError(common.LogText("QYDION 补入账失败 trade_no=%s: %v", tradeNo, err))
			common.ApiErrorMsg(c, "入账失败，请查看系统日志")
			return
		}
	} else if err := service.QydionResettleCancelledOrder(tradeNo, operator, "管理补入账（渠道已查证付款）"); err != nil {
		common.SysError(common.LogText("QYDION 补入账失败 trade_no=%s: %v", tradeNo, err))
		common.ApiErrorMsg(c, "入账失败，请查看系统日志")
		return
	}
	model.RecordLog(topUp.UserId, model.LogTypeTopup, common.NewMessage("充值 {{money}} 元已到账（管理员凭渠道查证补入账）", map[string]any{"money": fmt.Sprintf("%.2f", topUp.Money)}))
	common.SysLog(common.LogText("QYDION 管理补入账 trade_no=%s provider=%s operator=%s", tradeNo, topUp.PaymentProvider, operator))
	common.ApiSuccess(c, gin.H{"trade_no": tradeNo, "settled_by": operator})
}

// qydionNotifyAck 按渠道规范应答（支付宝 success/fail；微信 JSON code；Stripe 200/400）
func qydionNotifyAck(c *gin.Context, providerName string, ok bool) {
	switch providerName {
	case "alipay_f2f":
		if ok {
			c.String(http.StatusOK, "success")
		} else {
			c.String(http.StatusOK, "fail") // 支付宝对非 success 会重试
		}
	case "wx_native":
		if ok {
			c.JSON(http.StatusOK, gin.H{"code": "SUCCESS"})
		} else {
			c.JSON(http.StatusInternalServerError, gin.H{"code": "FAIL", "message": "verify or settle failed"})
		}
	default: // stripe 等
		if ok {
			c.Status(http.StatusOK)
		} else {
			c.Status(http.StatusBadRequest)
		}
	}
}

// ---------- 管理侧：支付设置（填参数即启用） ----------

// QydionGetPaymentSettings 管理侧：读支付参数（密钥类只回「已设置+尾4位」）
func QydionGetPaymentSettings(c *gin.Context) {
	result := map[string]string{}
	for key := range service.QydionPaymentSettingKeys {
		v := service.QydionSettingStr(key, "")
		if v == "" {
			result[key] = ""
			continue
		}
		if service.QydionPaymentSecretKeys[key] && len(v) > 8 {
			result[key] = "已设置（尾号 " + v[len(v)-4:] + "）"
		} else {
			result[key] = v
		}
	}
	common.ApiSuccess(c, result)
}

// QydionSavePaymentSettings 管理侧：写支付参数（白名单键；空值=清空该项）
func QydionSavePaymentSettings(c *gin.Context) {
	var req map[string]string
	if err := c.ShouldBindJSON(&req); err != nil || len(req) == 0 {
		common.ApiErrorMsg(c, "参数无效")
		return
	}
	for key, val := range req {
		if !service.QydionPaymentSettingKeys[key] {
			common.ApiErrorMsg(c, "不支持的配置项："+key)
			return
		}
		val = strings.TrimSpace(val)
		if len(val) > 8192 {
			common.ApiErrorMsg(c, "配置值过长："+key)
			return
		}
		if err := model.UpdateOption(key, val); err != nil {
			common.SysError(common.LogText("QYDION 支付设置保存失败 %s: %v", key, err))
			common.ApiErrorMsg(c, "保存失败："+key)
			return
		}
	}
	service.QydionSettingsBustCache()
	common.SysLog(common.LogText("QYDION 支付设置已更新（%d 项，操作者 %s）", len(req), c.GetString("username")))
	common.ApiSuccess(c, gin.H{"updated": len(req)})
}

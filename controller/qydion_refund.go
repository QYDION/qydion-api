package controller

// QYDION 退款管理 API（交接报告 §E）：管理员创建退款 / 退款列表（分页含订单信息）/
// 悬挂退款单关闭兜底 / 用户侧查询本人退款记录。
// 业务规则与事务在 service/qydion_refund.go；这里只做参数校验、渠道组合与编排：
//   PrepareRefund（事务A：校验+预扣+pending 落库）→ 渠道退款 API（事务外，
//   SQLite 单写者不能把网络请求包进事务，同 qydion_credit.go 的纪律）→
//   CompleteRefund / FailRefund（事务B：定稿+回补）。

import (
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/payment"
	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
)

// QydionCreateRefund 管理侧：创建退款（管理员审核即执行）。
// body: { "trade_no": "QYD...", "refund_money": 10.5, "reason": "可选" }
// refund_money 为人民币元，两位小数；多次部分退款累计不得超过订单实付。
func QydionCreateRefund(c *gin.Context) {
	var req struct {
		TradeNo     string  `json:"trade_no"`
		RefundMoney float64 `json:"refund_money"`
		Reason      string  `json:"reason"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || strings.TrimSpace(req.TradeNo) == "" {
		common.ApiErrorMsg(c, "参数无效（需要 trade_no 与 refund_money）")
		return
	}
	// 金额必须 >0 且为两位小数（与下单同一纪律，杜绝 0.001 元微额单刷退款单）
	if req.RefundMoney <= 0 {
		common.ApiErrorMsg(c, "退款金额必须大于 0")
		return
	}
	if req.RefundMoney != qydionRoundMoney(req.RefundMoney) {
		common.ApiErrorMsg(c, "退款金额必须为两位小数（元）")
		return
	}
	tradeNo := strings.TrimSpace(req.TradeNo)
	refundCents := int64(math.Round(req.RefundMoney * 100))
	operator := c.GetString("username")
	if operator == "" {
		operator = fmt.Sprintf("admin#%d", c.GetInt("id"))
	}

	// 事务A：校验（渠道/状态/封顶/未消耗约束）+ 预扣额度 + pending 落库
	record, err := service.QydionPrepareRefund(tradeNo, refundCents, operator)
	if err != nil {
		common.SysError(common.LogText("QYDION 退款预校验/预扣失败 trade_no=%s operator=%s: %v", tradeNo, operator, err))
		common.ApiErrorMsg(c, err.Error())
		return
	}
	topUp := model.GetTopUpByTradeNo(tradeNo)
	if topUp == nil {
		// 理论不可达（事务A刚锁过该单）；兜底回补，绝不留下扣了额度的孤儿单
		if ferr := service.QydionFailRefund(record.Id, "系统错误：预扣后订单读取失败"); ferr != nil {
			common.SysError(common.LogText("QYDION 退款兜底回补失败 refund=%d: %v", record.Id, ferr))
		}
		common.ApiErrorMsg(c, "订单读取失败，已回补额度")
		return
	}

	// 渠道组合：未注册/未配置/不支持一律明确报错并回补，绝不假成功
	provider := payment.Get(topUp.PaymentProvider)
	if provider == nil || !provider.Enabled() {
		if ferr := service.QydionFailRefund(record.Id, "渠道未配置或未启用"); ferr != nil {
			common.SysError(common.LogText("QYDION 退款回补失败 refund=%d: %v", record.Id, ferr))
		}
		common.ApiErrorMsg(c, fmt.Sprintf("渠道 %s 未配置或未启用，无法在线退款（额度已回补）", topUp.PaymentProvider))
		return
	}
	refunder, ok := provider.(payment.Refunder)
	if !ok {
		if ferr := service.QydionFailRefund(record.Id, "渠道不支持在线退款"); ferr != nil {
			common.SysError(common.LogText("QYDION 退款回补失败 refund=%d: %v", record.Id, ferr))
		}
		common.ApiErrorMsg(c, "该渠道不支持在线退款（额度已回补）")
		return
	}
	result, err := refunder.Refund(payment.RefundRequest{
		TradeNo:      tradeNo,
		OutRequestNo: record.OutRequestNo,
		RefundCents:  refundCents,
		TotalCents:   int64(math.Round(topUp.Money * 100)),
		Reason:       strings.TrimSpace(req.Reason),
	})
	if err != nil || !result.Succeeded {
		reason := "渠道退款失败"
		if err != nil {
			reason = err.Error()
		}
		if ferr := service.QydionFailRefund(record.Id, reason); ferr != nil {
			common.SysError(common.LogText("QYDION 退款失败定稿异常 refund=%d: %v（渠道原因：%s）", record.Id, ferr, reason))
			common.ApiErrorMsg(c, "退款失败且回补异常，请立即人工核对退款单 #"+fmt.Sprintf("%d", record.Id))
			return
		}
		common.SysError(common.LogText("QYDION 渠道退款失败 trade_no=%s refund=%d operator=%s: %s", tradeNo, record.Id, operator, reason))
		common.ApiErrorMsg(c, "退款失败（额度已回补）："+reason)
		return
	}
	if err := service.QydionCompleteRefund(record.Id, result.Credential, result.ChannelStatus); err != nil {
		// 渠道已受理但本地定稿失败：余额与退款单需要人工核对，明确报错不吞掉
		common.SysError(common.LogText("QYDION 退款定稿失败（渠道已受理）refund=%d credential=%s: %v", record.Id, result.Credential, err))
		common.ApiErrorMsg(c, fmt.Sprintf("渠道已受理退款，但本地登记失败，请人工核对退款单 #%d（凭证 %s）", record.Id, result.Credential))
		return
	}
	common.ApiSuccess(c, gin.H{
		"id":                 record.Id,
		"trade_no":           tradeNo,
		"out_request_no":     record.OutRequestNo,
		"refund_cents":       record.RefundCents,
		"refund_quota":       record.RefundQuota,
		"status":             model.RefundStatusSucceeded,
		"channel":            record.Channel,
		"channel_credential": result.Credential,
	})
}

// QydionListRefunds 管理侧：退款列表（分页，含订单信息与用户名）。
// query: p（页码）、page_size（默认 20，≤100）、trade_no（可选，按订单号精确过滤）
func QydionListRefunds(c *gin.Context) {
	page, _ := strconv.Atoi(c.DefaultQuery("p", "1"))
	if page < 1 {
		page = 1
	}
	pageSize, _ := strconv.Atoi(c.DefaultQuery("page_size", "20"))
	if pageSize < 1 || pageSize > 100 {
		pageSize = 20
	}
	tradeNo := strings.TrimSpace(c.Query("trade_no"))
	list, total, err := model.QydionListRefundsPage(page, pageSize, tradeNo)
	if err != nil {
		common.SysError(common.LogText("QYDION 退款列表查询失败: %v", err))
		common.ApiErrorMsg(c, "查询失败，请稍后重试")
		return
	}
	// 逐行补订单信息与用户名（对齐 QydionListReviewQueue 的按行补全风格）
	type refundRow struct {
		model.QydionRefund
		Username      string  `json:"username"`
		OrderMoney    float64 `json:"order_money"`    // 订单实付（元）
		OrderStatus   string  `json:"order_status"`   // 订单当前状态
		QuotaSnapshot int64   `json:"quota_snapshot"` // 订单到账额度快照（N07）
	}
	rows := make([]refundRow, 0, len(list))
	for _, r := range list {
		row := refundRow{QydionRefund: r}
		var user model.User
		if err := model.DB.Select("username").Where("id = ?", r.UserId).First(&user).Error; err == nil {
			row.Username = user.Username
		}
		if t := model.GetTopUpByTradeNo(r.TradeNo); t != nil {
			row.OrderMoney = t.Money
			row.OrderStatus = t.Status
			row.QuotaSnapshot = t.QuotaSnapshot
		}
		rows = append(rows, row)
	}
	common.ApiSuccess(c, gin.H{"items": rows, "total": total, "page": page, "page_size": pageSize})
}

// QydionCloseRefund 管理侧：关闭悬挂的 pending 退款单（进程中断兜底）。
// 仅 pending 可关：关闭即按预扣额原额回补（与渠道失败同一事务语义）。
// 渠道侧是否实际退款以渠道对账为准——本接口只清本地挂起态。
// body: { "refund_id": 1 }
func QydionCloseRefund(c *gin.Context) {
	var req struct {
		RefundId int64 `json:"refund_id"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || req.RefundId <= 0 {
		common.ApiErrorMsg(c, "参数无效（需要 refund_id）")
		return
	}
	existing, err := model.QydionGetRefundById(req.RefundId)
	if err != nil || existing == nil {
		common.ApiErrorMsg(c, "退款单不存在")
		return
	}
	if existing.Status != model.RefundStatusPending {
		common.ApiErrorMsg(c, "仅 pending 状态的退款单可关闭")
		return
	}
	operator := c.GetString("username")
	if operator == "" {
		operator = fmt.Sprintf("admin#%d", c.GetInt("id"))
	}
	if err := service.QydionFailRefund(req.RefundId, "管理员关闭悬挂退款单（操作者 "+operator+"）"); err != nil {
		common.SysError(common.LogText("QYDION 悬挂退款单关闭失败 refund=%d operator=%s: %v", req.RefundId, operator, err))
		common.ApiErrorMsg(c, "关闭失败："+err.Error())
		return
	}
	common.SysLog(common.LogText("QYDION 悬挂退款单已关闭 refund=%d operator=%s", req.RefundId, operator))
	common.ApiSuccess(c, gin.H{"id": req.RefundId, "status": model.RefundStatusFailed})
}

// QydionListMyRefunds 用户侧：本人退款记录（时间倒序，最多 50 条）
func QydionListMyRefunds(c *gin.Context) {
	userId := c.GetInt("id")
	list, err := model.QydionListUserRefunds(userId, 50)
	if err != nil {
		common.SysError(common.LogText("QYDION 用户退款记录查询失败 user=%d: %v", userId, err))
		common.ApiErrorMsg(c, "查询失败，请稍后重试")
		return
	}
	common.ApiSuccess(c, list)
}

package controller

// QYDION 对公转账人工流（§8.6c）+ 支付渠道设置视图（§8.6b）

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/payment"
	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// QydionGetCorporateAccount 用户侧：取对公收款账户信息（后台选项配置）
func QydionGetCorporateAccount(c *gin.Context) {
	provider := payment.Get("corporate")
	enabled := provider != nil && provider.Enabled()
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"data": gin.H{
			"enabled":      enabled,
			"account_name": service.QydionSettingStr(service.QydionKeyCorpAccountName, ""),
			"account_no":   service.QydionSettingStr(service.QydionKeyCorpAccountNo, ""),
			"bank_name":    service.QydionSettingStr(service.QydionKeyCorpBankName, ""),
			"note":         service.QydionSettingStr(service.QydionKeyCorpNote, ""),
			"price":        1, // 汇率口径由订单计算时折算
		},
	})
}

// QydionCreateCorporateTopup 用户侧：创建对公充值订单（进 reviewing）+ 上传凭证
// form: money（人民币实付金额）, coupon_code（可选）, serial_suffix（流水号后6位）, file（jpg/png/pdf ≤5MB）
func QydionCreateCorporateTopup(c *gin.Context) {
	userId := c.GetInt("id")
	money, err := strconv.ParseFloat(strings.TrimSpace(c.PostForm("money")), 64)
	if err != nil || money <= 0 {
		common.ApiErrorMsg(c, "充值金额无效")
		return
	}
	// 全量安全审计 P3：金额限定两位小数且 ≥1 元——防止 0.000001 元微额单卡死审核队列
	if money != float64(int64(money*100+0.5))/100 || money < 1 {
		common.ApiErrorMsg(c, "充值金额必须为不小于 1 元的两位小数")
		return
	}
	if money > 1000000 {
		common.ApiErrorMsg(c, "单笔金额超限，请联系客服走大额通道")
		return
	}
	// 全量安全审计 P2：每用户每日提交次数上限（防认证用户脚本化灌爆凭证存储与审核队列）
	var todayCnt int64
	dayStart := time.Now().Truncate(24 * time.Hour).Unix()
	if err := model.DB.Model(&model.TopUp{}).
		Where("user_id = ? AND payment_provider = ? AND create_time >= ?", userId, "corporate", dayStart).
		Count(&todayCnt).Error; err == nil && todayCnt >= 20 {
		common.ApiErrorMsg(c, "今日对公充值提交次数已达上限（20 次/天），如有需要请联系客服")
		return
	}
	serialSuffix := strings.TrimSpace(c.PostForm("serial_suffix"))
	if len(serialSuffix) != 6 {
		common.ApiErrorMsg(c, "请填写转账流水号后 6 位")
		return
	}

	// 凭证校验：jpg/png/pdf ≤5MB
	fileHeader, err := c.FormFile("file")
	if err != nil {
		common.ApiErrorMsg(c, "请上传转账凭证截图")
		return
	}
	if fileHeader.Size > 5*1024*1024 {
		common.ApiErrorMsg(c, "凭证文件不能超过 5MB")
		return
	}
	ext := strings.ToLower(filepath.Ext(fileHeader.Filename))
	if ext != ".jpg" && ext != ".jpeg" && ext != ".png" && ext != ".pdf" {
		common.ApiErrorMsg(c, "凭证仅支持 jpg/png/pdf")
		return
	}

	// 优惠券（可选）：校验 → 预占 → 折减金额（与在线下单同一套状态机，防一券挂多单）
	payMoney := money
	var coupon *model.QydionCoupon
	if code := strings.TrimSpace(c.PostForm("coupon_code")); code != "" {
		var msg string
		coupon, msg = model.ValidateCoupon(code, userId, money)
		if coupon == nil {
			common.ApiErrorMsg(c, msg)
			return
		}
		locked, err := model.LockCoupon(coupon.Id, userId)
		if err != nil {
			common.SysError(common.LogText("QYDION 对公券预占失败 coupon=%d: %v", coupon.Id, err))
			common.ApiErrorMsg(c, "优惠码占用失败，请稍后重试")
			return
		}
		if !locked {
			common.ApiErrorMsg(c, "该优惠码刚被其他订单占用，请重新下单或更换优惠码")
			return
		}
		payMoney = qydionRoundMoney(model.QydionCouponAmount(coupon, money))
		// 全量安全审计 P3：券折后实付为 0 的订单无法入账（qydionMoneyToQuota 拒绝 0），
		// 直接拒绝创建，避免产生永远无法通过的"僵尸审核单"
		if payMoney <= 0 {
			_ = model.ReleaseCoupon(coupon.Id)
			common.ApiErrorMsg(c, "优惠券已抵扣全部金额，无需走对公转账（请调整金额或取消用券）")
			return
		}
	}

	// 保存凭证（随机 UUID 文件名；下载仅管理员鉴权 §8.6c）
	saveName := uuid.NewString() + ext
	saveDir := filepath.Join("uploads", "vouchers")
	if err := os.MkdirAll(saveDir, 0755); err != nil {
		common.ApiErrorMsg(c, "凭证存储初始化失败")
		return
	}
	savePath := filepath.Join(saveDir, saveName)
	if err := c.SaveUploadedFile(fileHeader, savePath); err != nil {
		common.ApiErrorMsg(c, "凭证保存失败")
		return
	}

	// 全量安全审计 P3：单号随机源换 crypto/rand（资金凭证纵深防御；math/rand 虽已
	// 由 ChaCha8 播种，但券码/单号类资产按规范不应使用非加密随机源）
	tradeNo := fmt.Sprintf("CORP%d%06d", time.Now().Unix(), qydionSecureInt(1000000))
	snapshot, err := service.QydionQuotaForMoney(payMoney)
	if err != nil {
		releaseQydionCoupon(coupon)
		common.ApiErrorMsg(c, "充值额度计算失败，请联系管理员")
		return
	}
	topUp := &model.TopUp{
		UserId:          userId,
		Amount:          0,
		Money:           payMoney,
		TradeNo:         tradeNo,
		PaymentMethod:   "corporate",
		PaymentProvider: "corporate",
		CreateTime:      time.Now().Unix(),
		Status:          service.TopupStatusReviewing,
		QuotaSnapshot:   snapshot,
	}
	if coupon != nil {
		topUp.CouponId = coupon.Id
	}
	if err := topUp.Insert(); err != nil {
		_ = os.Remove(savePath) // 全量安全审计 P2：入库失败清理已落盘凭证，避免孤儿文件
		releaseQydionCoupon(coupon)
		common.ApiErrorMsg(c, "订单创建失败")
		return
	}
	voucher := model.QydionTopupVoucher{
		TradeNo: tradeNo, FilePath: savePath, OriginalName: fileHeader.Filename,
		SerialSuffix: serialSuffix, UploadedBy: userId, UploadedAt: time.Now(),
	}
	if err := model.DB.Create(&voucher).Error; err != nil {
		_ = os.Remove(savePath)
		common.ApiErrorMsg(c, "凭证登记失败")
		return
	}
	common.ApiSuccess(c, gin.H{"trade_no": tradeNo, "money": payMoney, "status": topUp.Status})
}

// QydionListMyCorporateTopups 用户侧：我的对公订单
func QydionListMyCorporateTopups(c *gin.Context) {
	userId := c.GetInt("id")
	var items []model.TopUp
	model.DB.Where("user_id = ? AND payment_provider = ?", userId, "corporate").
		Order("id DESC").Limit(50).Find(&items)
	common.ApiSuccess(c, items)
}

// qydionReviewItem 审核队列行（订单+凭证+用户+历史充值次数）
type qydionReviewItem struct {
	model.TopUp
	Username     string `json:"username"`
	VoucherName  string `json:"voucher_name"`
	SerialSuffix string `json:"serial_suffix"`
	TopupCount   int64  `json:"topup_count"`
	HasVoucher   bool   `json:"has_voucher"`
}

// QydionListReviewQueue 管理侧：充值审核队列（reviewing）
func QydionListReviewQueue(c *gin.Context) {
	var items []model.TopUp
	model.DB.Where("status = ? AND payment_provider = ?", service.TopupStatusReviewing, "corporate").
		Order("id ASC").Limit(100).Find(&items)
	result := make([]qydionReviewItem, 0, len(items))
	for _, t := range items {
		item := qydionReviewItem{TopUp: t}
		var user model.User
		if err := model.DB.Select("username").Where("id = ?", t.UserId).First(&user).Error; err == nil {
			item.Username = user.Username
		}
		var voucher model.QydionTopupVoucher
		if err := model.DB.Where("trade_no = ?", t.TradeNo).First(&voucher).Error; err == nil {
			item.HasVoucher = true
			item.VoucherName = voucher.OriginalName
			item.SerialSuffix = voucher.SerialSuffix
		}
		model.DB.Model(&model.TopUp{}).Where("user_id = ? AND status = ?", t.UserId, common.TopUpStatusSuccess).Count(&item.TopupCount)
		result = append(result, item)
	}
	common.ApiSuccess(c, result)
}

// QydionReviewTopup 管理侧：审核（approve → 统一入账；reject 必填原因）
// body: { "trade_no": "...", "action": "approve"|"reject", "reason": "..." }
func QydionReviewTopup(c *gin.Context) {
	var req struct {
		TradeNo string `json:"trade_no"`
		Action  string `json:"action"`
		Reason  string `json:"reason"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || req.TradeNo == "" {
		common.ApiErrorMsg(c, "参数无效")
		return
	}
	topUp := model.GetTopUpByTradeNo(req.TradeNo)
	if topUp == nil {
		common.ApiErrorMsg(c, "订单不存在")
		return
	}
	// 全量安全审计 P3：审核接口只受理对公订单，防止其他渠道单号误入本流程
	if topUp.PaymentProvider != "corporate" {
		common.ApiErrorMsg(c, "非对公转账订单，不能在此审核")
		return
	}
	operator := c.GetString("username")
	if operator == "" {
		operator = fmt.Sprintf("admin#%d", c.GetInt("id"))
	}
	switch req.Action {
	case "approve":
		alreadyDone, err := service.QydionCreditCorporateOrder(topUp, operator, "对公审核通过")
		if err != nil {
			// 全量安全审计 P3：错误细节（含 SQL 片段/表名）不回显管理端，落系统日志
			common.SysError(common.LogText("QYDION 对公入账失败 trade_no=%s: %v", req.TradeNo, err))
			common.ApiErrorMsg(c, "入账失败，请稍后重试或查看系统日志")
			return
		}
		if alreadyDone {
			// 重复 approve：不重复发通知/日志（全量安全审计 P3）
			common.ApiSuccess(c, gin.H{"status": "success", "already_done": true})
			return
		}
		model.RecordLog(topUp.UserId, model.LogTypeTopup, common.NewMessage("对公充值审核通过，金额 {{money}} 元已到账", map[string]any{"money": fmt.Sprintf("%.2f", topUp.Money)}))
		qydionNotifyUserEmail(topUp.UserId, "对公充值审核通过",
			fmt.Sprintf("您的对公充值订单 %s（%.2f 元）已审核通过并到账。", topUp.TradeNo, topUp.Money))
		common.ApiSuccess(c, gin.H{"status": "success"})
	case "reject":
		if strings.TrimSpace(req.Reason) == "" {
			common.ApiErrorMsg(c, "驳回必须填写原因（用户可见）")
			return
		}
		result := model.DB.Model(&model.TopUp{}).
			Where("trade_no = ? AND status = ?", req.TradeNo, service.TopupStatusReviewing).
			Update("status", service.TopupStatusRejected)
		if result.Error != nil || result.RowsAffected == 0 {
			common.ApiErrorMsg(c, "驳回失败：订单不存在或状态已变更")
			return
		}
		model.RecordLog(topUp.UserId, model.LogTypeTopup, common.NewMessage("对公充值被驳回：{{reason}}", map[string]any{"reason": req.Reason}))
		// 驳回后释放预占的券，否则用户改完凭证重新下单时这张券会一直锁在死单上
		releaseQydionCouponId(topUp.CouponId)
		qydionNotifyUserEmail(topUp.UserId, "对公充值审核未通过",
			fmt.Sprintf("您的对公充值订单 %s 未通过审核。原因：%s", topUp.TradeNo, req.Reason))
		common.ApiSuccess(c, gin.H{"status": service.TopupStatusRejected})
	default:
		common.ApiErrorMsg(c, "action 仅支持 approve/reject")
	}
}

// QydionDownloadVoucher 管理侧：下载凭证（仅管理员，§8.6c）
func QydionDownloadVoucher(c *gin.Context) {
	tradeNo := c.Param("trade_no")
	var voucher model.QydionTopupVoucher
	if err := model.DB.Where("trade_no = ?", tradeNo).First(&voucher).Error; err != nil {
		common.ApiErrorMsg(c, "凭证不存在")
		return
	}
	if _, err := os.Stat(voucher.FilePath); err != nil {
		common.ApiErrorMsg(c, "凭证文件已丢失")
		return
	}
	c.FileAttachment(voucher.FilePath, voucher.OriginalName)
}

// QydionListPaymentProviders 管理侧：支付渠道列表（名称/启用状态；密钥走后台选项，不在本接口回显）
func QydionListPaymentProviders(c *gin.Context) {
	providers := payment.ListAll()
	result := make([]gin.H, 0, len(providers))
	for _, p := range providers {
		result = append(result, gin.H{"name": p.Name(), "display_name": p.DisplayName(), "enabled": p.Enabled()})
	}
	common.ApiSuccess(c, result)
}

// qydionNotifyUserEmail 站内邮件通知（§8.10；SMTP 未配置时静默跳过）
func qydionNotifyUserEmail(userId int, subject, content string) {
	var user model.User
	if err := model.DB.Select("email").Where("id = ?", userId).First(&user).Error; err != nil || user.Email == "" {
		return
	}
	go func() {
		defer func() { _ = recover() }()
		_ = common.SendEmail(subject, user.Email, content)
	}()
}

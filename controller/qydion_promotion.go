package controller

// QYDION 促销系统（§8.7）：活动管理 + 优惠券 + 用户侧接口

import (
	crand "crypto/rand"
	"encoding/json"
	"fmt"
	"math/big"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/gin-gonic/gin"
)

// ---------- 活动管理（§8.7a） ----------

// QydionListCampaigns 管理侧：活动列表（status 筛选 all/draft/active/ended）
func QydionListCampaigns(c *gin.Context) {
	status := strings.TrimSpace(c.Query("status"))
	var list []model.QydionCampaign
	db := model.DB.Order("id DESC").Limit(200)
	if status != "" && status != "all" {
		db = db.Where("status = ?", status)
	}
	if err := db.Find(&list).Error; err != nil {
		common.ApiErrorMsg(c, "查询失败")
		return
	}
	now := time.Now()
	result := make([]gin.H, 0, len(list))
	for _, camp := range list {
		// 运行时态修正显示：active 且已过期的按 ended 展示
		displayStatus := camp.Status
		if camp.Status == model.CampaignStatusActive && now.After(camp.EndsAt) {
			displayStatus = model.CampaignStatusEnded
		}
		result = append(result, gin.H{
			"id": camp.Id, "name": camp.Name, "type": camp.Type,
			"starts_at": camp.StartsAt, "ends_at": camp.EndsAt,
			"rule": camp.Rule, "status": camp.Status, "display_status": displayStatus,
			"budget_quota": camp.BudgetQuota, "used_quota": camp.UsedQuota,
			"per_user_limit": camp.PerUserLimit, "note": camp.Note,
		})
	}
	common.ApiSuccess(c, result)
}

// QydionSaveCampaign 管理侧：新建/编辑活动（id=0 新建）
func QydionSaveCampaign(c *gin.Context) {
	var req model.QydionCampaign
	if err := c.ShouldBindJSON(&req); err != nil {
		common.ApiErrorMsg(c, "参数无效")
		return
	}
	switch req.Type {
	case model.CampaignTypeTopupBonus, model.CampaignTypeCouponBatch,
		model.CampaignTypeInviteBoost, model.CampaignTypeNewUserGift:
	default:
		common.ApiErrorMsg(c, "活动类型无效")
		return
	}
	if strings.TrimSpace(req.Name) == "" {
		common.ApiErrorMsg(c, "活动名称不能为空")
		return
	}
	if !req.EndsAt.After(req.StartsAt) {
		common.ApiErrorMsg(c, "结束时间必须晚于开始时间")
		return
	}
	// Rule JSON 合法性校验
	if strings.TrimSpace(req.Rule) != "" {
		var tmp any
		if err := json.Unmarshal([]byte(req.Rule), &tmp); err != nil {
			common.ApiErrorMsg(c, "规则必须是合法 JSON")
			return
		}
	} else {
		req.Rule = "{}"
	}
	if req.Id > 0 {
		// 编辑不隐式改状态：未传 status 时保留原值（E2E 发现的清空 bug 修复）
		var existing model.QydionCampaign
		if err := model.DB.First(&existing, req.Id).Error; err != nil {
			common.ApiErrorMsg(c, "活动不存在")
			return
		}
		if strings.TrimSpace(req.Status) == "" {
			req.Status = existing.Status
		}
		if err := req.Update(); err != nil {
			common.ApiErrorMsg(c, "保存失败："+err.Error())
			return
		}
	} else {
		req.Status = model.CampaignStatusDraft
		if err := req.Insert(); err != nil {
			common.ApiErrorMsg(c, "创建失败："+err.Error())
			return
		}
	}
	common.ApiSuccess(c, req)
}

// QydionSetCampaignStatus 管理侧：状态流转（激活/结束/手动终止）
func QydionSetCampaignStatus(c *gin.Context) {
	var req struct {
		Id     int    `json:"id"`
		Status string `json:"status"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		common.ApiErrorMsg(c, "参数无效")
		return
	}
	switch req.Status {
	case model.CampaignStatusActive, model.CampaignStatusEnded, model.CampaignStatusTerminated:
	default:
		common.ApiErrorMsg(c, "目标状态无效（active/ended/terminated）")
		return
	}
	result := model.DB.Model(&model.QydionCampaign{}).Where("id = ?", req.Id).Update("status", req.Status)
	if result.Error != nil || result.RowsAffected == 0 {
		common.ApiErrorMsg(c, "活动不存在或状态未变更")
		return
	}
	common.ApiSuccess(c, nil)
}

// ---------- 优惠券（§8.7c） ----------

// QydionGenerateCouponBatch 管理侧：批次生成
// body: { "count": 100, "prefix": "QYDION", "type": "amount"|"discount", "value": 10,
//         "min_amount": 0, "expires_at": "2026-12-31T00:00:00Z", "bound_user": 0 }
func QydionGenerateCouponBatch(c *gin.Context) {
	var req struct {
		Count     int       `json:"count"`
		Prefix    string    `json:"prefix"`
		Type      string    `json:"type"`
		Value     int64     `json:"value"`
		MinAmount int64     `json:"min_amount"`
		ExpiresAt time.Time `json:"expires_at"`
		BoundUser int       `json:"bound_user"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		common.ApiErrorMsg(c, "参数无效")
		return
	}
	if req.Count < 1 || req.Count > 10000 {
		common.ApiErrorMsg(c, "单批数量限 1-10000")
		return
	}
	if req.Type != model.CouponTypeAmount && req.Type != model.CouponTypeDiscount {
		common.ApiErrorMsg(c, "类型仅支持 amount/discount")
		return
	}
	if req.Value <= 0 {
		common.ApiErrorMsg(c, "面值/折扣率必须大于 0")
		return
	}
	if req.Type == model.CouponTypeDiscount && (req.Value >= 100 || req.Value <= 0) {
		common.ApiErrorMsg(c, "折扣率取值 1-99（如 90 表示九折）")
		return
	}
	req.Prefix = strings.ToUpper(strings.TrimSpace(req.Prefix))
	if req.Prefix == "" {
		req.Prefix = "QYDION"
	}

	var batch model.QydionCampaign
	batch.Name = fmt.Sprintf("优惠券批次 %s %d 张", req.Prefix, req.Count)
	batch.Type = model.CampaignTypeCouponBatch
	batch.Status = model.CampaignStatusActive
	batch.StartsAt = time.Now()
	if req.ExpiresAt.IsZero() {
		batch.EndsAt = time.Now().AddDate(1, 0, 0)
		req.ExpiresAt = batch.EndsAt
	} else {
		batch.EndsAt = req.ExpiresAt
	}
	batch.Rule = "{}"
	batch.Note = "系统生成"
	if err := batch.Insert(); err != nil {
		common.ApiErrorMsg(c, "批次创建失败")
		return
	}

	codes := make([]model.QydionCoupon, 0, req.Count)
	for i := 0; i < req.Count; i++ {
		code := fmt.Sprintf("%s-%s", req.Prefix, qydionRandomCode(12))
		codes = append(codes, model.QydionCoupon{
			BatchId: batch.Id, Code: code, Type: req.Type, Value: req.Value,
			MinAmount: req.MinAmount, ExpiresAt: req.ExpiresAt,
			Status: model.CouponStatusUnused, BoundUser: req.BoundUser,
		})
	}
	if err := model.DB.CreateInBatches(codes, 500).Error; err != nil {
		common.ApiErrorMsg(c, "券码生成失败："+err.Error())
		return
	}
	common.ApiSuccess(c, gin.H{"batch_id": batch.Id, "count": len(codes), "codes": codes})
}

// QydionExportCouponBatch 管理侧：批次券码导出 CSV（营销发放用）
func QydionExportCouponBatch(c *gin.Context) {
	batchId, err := strconv.Atoi(c.Query("batch_id"))
	if err != nil || batchId <= 0 {
		common.ApiErrorMsg(c, "batch_id 无效")
		return
	}
	var coupons []model.QydionCoupon
	if err := model.DB.Where("batch_id = ?", batchId).Order("id ASC").Limit(20000).Find(&coupons).Error; err != nil {
		common.ApiErrorMsg(c, "查询失败")
		return
	}
	var sb strings.Builder
	sb.WriteString("code,type,value,min_amount,expires_at,status\n")
	for _, cp := range coupons {
		sb.WriteString(fmt.Sprintf("%s,%s,%d,%d,%s,%s\n",
			cp.Code, cp.Type, cp.Value, cp.MinAmount, cp.ExpiresAt.Format("2006-01-02"), cp.Status))
	}
	filename := fmt.Sprintf("coupons-batch-%d.csv", batchId)
	c.Header("Content-Disposition", "attachment; filename="+filename)
	c.Data(200, "text/csv; charset=utf-8", []byte(sb.String()))
}

// QydionVoidCoupon 管理侧：单张作废
func QydionVoidCoupon(c *gin.Context) {
	var req struct {
		Id int `json:"id"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || req.Id <= 0 {
		common.ApiErrorMsg(c, "参数无效")
		return
	}
	result := model.DB.Model(&model.QydionCoupon{}).
		Where("id = ? AND status = ?", req.Id, model.CouponStatusUnused).
		Update("status", model.CouponStatusDisabled)
	if result.Error != nil || result.RowsAffected == 0 {
		common.ApiErrorMsg(c, "作废失败：不存在或已使用")
		return
	}
	common.ApiSuccess(c, nil)
}

// QydionListCoupons 管理侧：券列表（按批次）
func QydionListCoupons(c *gin.Context) {
	batchId, err := strconv.Atoi(c.Query("batch_id"))
	if err != nil || batchId <= 0 {
		common.ApiErrorMsg(c, "batch_id 无效")
		return
	}
	var coupons []model.QydionCoupon
	model.DB.Where("batch_id = ?", batchId).Order("id ASC").Limit(500).Find(&coupons)
	common.ApiSuccess(c, coupons)
}

// QydionValidateCoupon 用户侧：充值页实时校验（应付金额联动）
// 防刷：同 IP 每分钟校验 ≤10 次（§8.7c，简单计数器）
func QydionValidateCoupon(c *gin.Context) {
	ip := c.ClientIP()
	if !qydionCouponRateLimit(ip) {
		common.ApiErrorMsg(c, "校验过于频繁，请稍后再试")
		return
	}
	var req struct {
		Code      string  `json:"code"`
		PayAmount float64 `json:"pay_amount"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		common.ApiErrorMsg(c, "参数无效")
		return
	}
	userId := c.GetInt("id")
	coupon, msg := model.ValidateCoupon(strings.TrimSpace(req.Code), userId, req.PayAmount)
	if coupon == nil {
		common.ApiErrorMsg(c, msg)
		return
	}
	common.ApiSuccess(c, gin.H{
		"id": coupon.Id, "type": coupon.Type, "value": coupon.Value,
		"final_amount": model.QydionCouponAmount(coupon, req.PayAmount),
	})
}

// QydionActiveTopupTiers 用户侧：充值页实时展示当前命中的满送档位（§8.7b 营销露出）
func QydionActiveTopupTiers(c *gin.Context) {
	campaigns, err := model.ActiveCampaigns(model.CampaignTypeTopupBonus)
	if err != nil {
		common.ApiSuccess(c, []gin.H{})
		return
	}
	result := make([]gin.H, 0, len(campaigns))
	for _, camp := range campaigns {
		result = append(result, gin.H{"id": camp.Id, "name": camp.Name, "rule": camp.Rule, "ends_at": camp.EndsAt})
	}
	common.ApiSuccess(c, result)
}

// ---------- 内部工具 ----------

// qydionSecureInt 全量安全审计 P3：资金凭证随机源统一 crypto/rand（券码/对公单号）
func qydionSecureInt(n int) int {
	if n <= 0 {
		return 0
	}
	v, err := crand.Int(crand.Reader, big.NewInt(int64(n)))
	if err != nil {
		return 0
	}
	return int(v.Int64())
}

func qydionRandomCode(n int) string {
	const chars = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789"
	b := make([]byte, n)
	for i := range b {
		b[i] = chars[qydionSecureInt(len(chars))]
	}
	return string(b)
}

// qydionCouponRateLimit 同 IP 每分钟 ≤10 次校验（进程内计数器，多实例部署时为每实例限额）
var qydionCouponRateMap syncMapOfInt64

type syncMapOfInt64 struct {
	mu sync.Mutex
	m  map[string]*qydionRateWindow
}

type qydionRateWindow struct {
	count int64
	reset time.Time
}

func (s *syncMapOfInt64) allow(key string, limit int64, window time.Duration) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.m == nil {
		s.m = map[string]*qydionRateWindow{}
	}
	w, ok := s.m[key]
	now := time.Now()
	if !ok || now.After(w.reset) {
		// 全量安全审计 P3：条目数封顶 + 过期清扫——IPv6 逐请求换源地址可让 map 无界膨胀
		if len(s.m) >= 8192 {
			for k, v := range s.m {
				if now.After(v.reset) {
					delete(s.m, k)
				}
			}
			if len(s.m) >= 8192 {
				return false // 清扫后仍满：拒绝新键，保护内存（已有键不受影响）
			}
		}
		s.m[key] = &qydionRateWindow{count: 1, reset: now.Add(window)}
		return true
	}
	w.count++
	return w.count <= limit
}

func qydionCouponRateLimit(ip string) bool {
	return qydionCouponRateMap.allow("coupon:"+ip, 10, time.Minute)
}

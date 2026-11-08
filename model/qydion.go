package model

// QYDION API 二开数据层（M4，规格书 §8.2b/§8.5c/§8.6/§8.7）
// 原则（§7.2-4）：不改原生表结构；QYDION 新表全部以 Qydion 前缀命名，经 AutoMigrate 建表。
// 伴生扩展表通过业务键（model_name / trade_no）与原生表关联。

import (
	"errors"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/shopspring/decimal"
	"gorm.io/gorm"
)

// ---------------- §8.2b 模型维护扩展 ----------------

// QydionModelExt 原生 models 表（rc47 已含 description/tags/status/名称规则）的
// QYDION 扩展字段：展示名、排序、计费说明。业务键 model_name 关联。
type QydionModelExt struct {
	Id          int    `json:"id"`
	ModelName   string `json:"model_name" gorm:"uniqueIndex;size:128"`
	DisplayName string `json:"display_name" gorm:"size:128"`
	SortOrder   int    `json:"sort_order"`
	PricingNote string `json:"pricing_note" gorm:"size:255"`
}

func (QydionModelExt) TableName() string { return "qydion_model_ext" }

func UpsertQydionModelExt(ext *QydionModelExt) error {
	if strings.TrimSpace(ext.ModelName) == "" {
		return errors.New("model_name 不能为空")
	}
	var count int64
	if err := DB.Model(&QydionModelExt{}).Where("model_name = ? AND id <> ?", ext.ModelName, ext.Id).Count(&count).Error; err != nil {
		return err
	}
	if count > 0 {
		return errors.New("该模型名已有扩展记录")
	}
	if ext.Id > 0 {
		return DB.Model(&QydionModelExt{}).Where("id = ?", ext.Id).
			Select("model_name", "display_name", "sort_order", "pricing_note").Updates(ext).Error
	}
	return DB.Create(ext).Error
}

func GetQydionModelExtList() ([]QydionModelExt, error) {
	var list []QydionModelExt
	err := DB.Order("sort_order ASC, id ASC").Find(&list).Error
	return list, err
}

func DeleteQydionModelExt(id int) error {
	return DB.Delete(&QydionModelExt{}, id).Error
}

// ---------------- §8.7a 活动管理 ----------------

const (
	CampaignTypeTopupBonus   = "topup_bonus"   // 充值满送 §8.7b
	CampaignTypeCouponBatch  = "coupon_batch"  // 优惠券批次 §8.7c
	CampaignTypeInviteBoost  = "invite_boost"  // 返佣预算 §8.7d
	CampaignTypeNewUserGift  = "new_user_gift" // 新客礼包 §8.7e
	CampaignStatusDraft      = "draft"
	CampaignStatusActive     = "active"
	CampaignStatusEnded      = "ended"
	CampaignStatusTerminated = "terminated" // 手动终止
)

// QydionCampaign 活动实体（§8.7a）。Rule 为 JSON，schema 按 Type 区分（见 API 文档）。
type QydionCampaign struct {
	Id           int       `json:"id"`
	Name         string    `json:"name" gorm:"size:128"`
	Type         string    `json:"type" gorm:"size:32;index"`
	StartsAt     time.Time `json:"starts_at"`
	EndsAt       time.Time `json:"ends_at"`
	Rule         string    `json:"rule" gorm:"type:text"`
	Status       string    `json:"status" gorm:"size:16;index;default:draft"`
	BudgetQuota  int64     `json:"budget_quota"` // 赠出总额度预算，0=不限
	UsedQuota    int64     `json:"used_quota"`
	PerUserLimit int       `json:"per_user_limit"` // 每用户可享次数，0=不限
	Note         string    `json:"note" gorm:"size:255"`
	CreatedTime  int64     `json:"created_time" gorm:"bigint"`
}

func (QydionCampaign) TableName() string { return "qydion_campaigns" }

// ActiveCampaigns 取当前生效（StartsAt ≤ now ≤ EndsAt 且 active）的指定类型活动。
func ActiveCampaigns(campaignType string) ([]QydionCampaign, error) {
	var list []QydionCampaign
	now := time.Now()
	err := DB.Where("type = ? AND status = ? AND starts_at <= ? AND ends_at >= ?", campaignType, CampaignStatusActive, now, now).
		Order("id ASC").Find(&list).Error
	return list, err
}

func GetCampaignById(id int) (*QydionCampaign, error) {
	var c QydionCampaign
	if err := DB.First(&c, id).Error; err != nil {
		return nil, err
	}
	return &c, nil
}

func (c *QydionCampaign) Insert() error {
	c.CreatedTime = time.Now().Unix()
	return DB.Create(c).Error
}

func (c *QydionCampaign) Update() error {
	return DB.Model(&QydionCampaign{}).Where("id = ?", c.Id).
		Select("name", "type", "starts_at", "ends_at", "rule", "status", "budget_quota", "per_user_limit", "note").
		Updates(c).Error
}

// ---------------- §8.7c 优惠券 ----------------

const (
	CouponTypeDiscount   = "discount" // 折扣率（value=百分比，如 90 表示九折）
	CouponTypeAmount     = "amount"   // 满减（value=人民币金额）
	CouponStatusUnused   = "unused"
	CouponStatusLocked   = "locked" // 下单即预占，尚未支付；关单回退 unused，入账转 used
	CouponStatusUsed     = "used"
	CouponStatusExpired  = "expired"
	CouponStatusDisabled = "disabled"
)

type QydionCoupon struct {
	Id        int       `json:"id"`
	BatchId   int       `json:"batch_id" gorm:"index"`
	Code      string    `json:"code" gorm:"uniqueIndex;size:64"`
	Type      string    `json:"type" gorm:"size:16"`
	Value     int64     `json:"value"`      // discount: 折扣率% (90=九折); amount: 满减金额（元）
	MinAmount int64     `json:"min_amount"` // 使用门槛（元），0=无门槛
	ExpiresAt time.Time `json:"expires_at"`
	Status    string    `json:"status" gorm:"size:16;index;default:unused"`
	BoundUser int       `json:"bound_user"` // 绑定指定用户；0=不限制
	UsedBy    int       `json:"used_by"`
	UsedAt    time.Time `json:"used_at"`
}

func (QydionCoupon) TableName() string { return "qydion_coupons" }

// ValidateCoupon 校验可用性：状态/过期/门槛/绑定用户。金额单位：人民币元。
// 只读校验——可用性到这里为止，真正的占用由 LockCoupon 完成，二者之间必然存在并发窗口，
// 所以调用方必须紧接着 LockCoupon，不能只凭本函数返回成功就下单收钱。
func ValidateCoupon(code string, userId int, payAmount float64) (*QydionCoupon, string) {
	var c QydionCoupon
	if err := DB.Where("code = ?", code).First(&c).Error; err != nil {
		return nil, "优惠码不存在"
	}
	if c.Status == CouponStatusUsed {
		return nil, "该优惠码已被使用"
	}
	if c.Status == CouponStatusLocked {
		return nil, "该优惠码已被订单占用，请先完成或取消那一笔订单"
	}
	if c.Status == CouponStatusDisabled {
		return nil, "该优惠码已作废"
	}
	if !c.ExpiresAt.IsZero() && time.Now().After(c.ExpiresAt) {
		return nil, "该优惠码已过期"
	}
	if c.BoundUser != 0 && c.BoundUser != userId {
		return nil, "该优惠码不适用于当前账号"
	}
	if c.MinAmount > 0 && payAmount < float64(c.MinAmount) {
		return nil, "订单金额未达到该优惠码使用门槛"
	}
	return &c, ""
}

// LockCoupon 下单即预占（unused → locked）。返回 false 表示这张券已被别的订单占住，
// 调用方必须当场拒绝下单——若只校验不占用，同一张券可以挂到多笔订单上，
// 每笔都按折后价付款，而结算时只有一笔能核销，其余「钱收到、入账被拒」，只能手工改库。
func LockCoupon(id int, userId int) (bool, error) {
	result := DB.Model(&QydionCoupon{}).
		Where("id = ? AND status = ?", id, CouponStatusUnused).
		Updates(map[string]any{"status": CouponStatusLocked, "used_by": userId})
	if result.Error != nil {
		return false, result.Error
	}
	return result.RowsAffected == 1, nil
}

// ReleaseCoupon 关单/下单失败时释放预占（locked → unused）。
// 只回退仍处 locked 的券：已 used 说明钱已到账，回退等于把一张券变成可反复消费。
func ReleaseCoupon(id int) error {
	return DB.Model(&QydionCoupon{}).
		Where("id = ? AND status = ?", id, CouponStatusLocked).
		Updates(map[string]any{"status": CouponStatusUnused, "used_by": 0}).Error
}

// CancelPendingOrderWithCoupon 原子关单：只把仍在 pending 的订单置为 cancelled，
// 并在同一事务内释放这一单预占的券。
//
// 分两步写会留下两种半态——订单已关但券仍 locked（那张券从此废掉），或券已放回
// 而订单仍可被后到的回调入账（一张券抵两笔）。RowsAffected<>1 说明订单已被其他
// 路径处理，此时绝不能动券。
func CancelPendingOrderWithCoupon(tradeNo string, couponId int) (bool, error) {
	cancelled := false
	err := DB.Transaction(func(tx *gorm.DB) error {
		result := tx.Model(&TopUp{}).
			Where("trade_no = ? AND status = ?", tradeNo, common.TopUpStatusPending).
			Update("status", common.TopUpStatusCancelled)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return nil
		}
		if couponId > 0 {
			if err := tx.Model(&QydionCoupon{}).
				Where("id = ? AND status = ?", couponId, CouponStatusLocked).
				Updates(map[string]any{"status": CouponStatusUnused, "used_by": 0}).Error; err != nil {
				return err
			}
		}
		cancelled = true
		return nil
	})
	return cancelled, err
}

// QydionCouponAmount 计算优惠后应付金额（人民币元）。
func QydionCouponAmount(c *QydionCoupon, payAmount float64) float64 {
	if c == nil {
		return payAmount
	}
	if c.Type == CouponTypeDiscount {
		ratio := decimal.NewFromInt(c.Value).Div(decimal.NewFromInt(100))
		got := decimal.NewFromFloat(payAmount).Mul(ratio)
		return got.InexactFloat64()
	}
	if c.Type == CouponTypeAmount {
		got := decimal.NewFromFloat(payAmount).Sub(decimal.NewFromInt(c.Value))
		if got.IsNegative() {
			return 0
		}
		return got.InexactFloat64()
	}
	return payAmount
}

// ---------------- §8.6d 额度流水（可审计） ----------------

const (
	QydionTxTypeTopup       = "topup"        // 充值到账
	QydionTxTypeBonus       = "bonus"        // 满送赠额
	QydionTxTypeCoupon      = "coupon"       // 优惠券抵扣
	QydionTxTypeRebate      = "rebate"       // 邀请返佣
	QydionTxTypeAdmin       = "admin"        // 管理员调整
	QydionTxTypeBonusExpire = "bonus_expire" // 赠额过期回收
)

// QydionQuotaTransaction 每一步一条流水，可审计（§8.6d-5）。
type QydionQuotaTransaction struct {
	Id           int64   `json:"id"`
	UserId       int     `json:"user_id" gorm:"index"`
	TradeNo      string  `json:"trade_no" gorm:"size:64;index"`
	Type         string  `json:"type" gorm:"size:16;index"`
	AmountQuota  int64   `json:"amount_quota"`  // 本笔额度变动（正=入账）
	Money        float64 `json:"money"`         // 关联人民币金额（如适用）
	BalanceAfter int64   `json:"balance_after"` // 变动后用户余额
	Operator     string  `json:"operator" gorm:"size:64"`
	Note         string  `json:"note" gorm:"size:255"`
	RefId        int     `json:"ref_id"` // 关联 campaign/coupon id
	CreatedTime  int64   `json:"created_time" gorm:"bigint;index"`
}

func (QydionQuotaTransaction) TableName() string { return "qydion_quota_transactions" }

// ---------------- §8.7b 满送赠额（带过期，cron 回收） ----------------

type QydionBonusGrant struct {
	Id          int64     `json:"id"`
	UserId      int       `json:"user_id" gorm:"index"`
	TradeNo     string    `json:"trade_no" gorm:"size:64;index"`
	CampaignId  int       `json:"campaign_id"`
	Amount      int64     `json:"amount"`
	ExpireAt    time.Time `json:"expire_at" gorm:"index"`
	Recovered   bool      `json:"recovered"`
	CreatedTime int64     `json:"created_time" gorm:"bigint"`
	// ConsumedWatermark 发放这批改额时用户累计已消费额度（users.used_quota）的快照。
	// 消费先抵赠额、抵完才动本金，所以过期可回收的量必须由「发放之后消费掉了多少」
	// 推算，而不是照原始金额扣：全额消费完的赠额到期再扣一遍，扣的是新充值的本金。
	ConsumedWatermark int64 `json:"consumed_watermark"`
}

func (QydionBonusGrant) TableName() string { return "qydion_bonus_grants" }

// ---------------- §8.5c 每日对账 ----------------

const (
	ReconStatusOk         = "ok"          // 差异在阈值内
	ReconStatusPending    = "pending"     // 待核对（超阈值）
	ReconStatusNoUpstream = "no_upstream" // 上游契约缺失（stub）
)

// QydionReconciliationDaily 对账结果表（§8.5c 表结构）。
type QydionReconciliationDaily struct {
	Id             int64   `json:"id"`
	Date           string  `json:"date" gorm:"size:10;uniqueIndex:uk_recon_channel_model"` // YYYY-MM-DD（Asia/Shanghai）
	ChannelId      int     `json:"channel_id" gorm:"uniqueIndex:uk_recon_channel_model"`
	Model          string  `json:"model" gorm:"size:128;uniqueIndex:uk_recon_channel_model"`
	LocalTokens    int64   `json:"local_tokens"`
	UpstreamTokens int64   `json:"upstream_tokens"`
	LocalAmount    float64 `json:"local_amount"`    // 本站计费折算成本（人民币）
	UpstreamAmount float64 `json:"upstream_amount"` // 上游账单金额（人民币）
	DiffRatio      float64 `json:"diff_ratio"`      // tokens 差异比例
	Status         string  `json:"status" gorm:"size:16"`
	Note           string  `json:"note" gorm:"size:255"`
	CreatedTime    int64   `json:"created_time" gorm:"bigint"`
}

func (QydionReconciliationDaily) TableName() string { return "qydion_reconciliation_daily" }

// ---------------- §8.6c 对公转账凭证 ----------------

type QydionTopupVoucher struct {
	Id           int64     `json:"id"`
	TradeNo      string    `json:"trade_no" gorm:"size:64;uniqueIndex"`
	FilePath     string    `json:"-" gorm:"size:255"` // 不回显路径，仅管理员经鉴权接口下载
	OriginalName string    `json:"original_name" gorm:"size:255"`
	SerialSuffix string    `json:"serial_suffix" gorm:"size:16"` // 流水号后6位
	UploadedBy   int       `json:"uploaded_by"`
	UploadedAt   time.Time `json:"uploaded_at"`
}

func (QydionTopupVoucher) TableName() string { return "qydion_topup_vouchers" }

// ---------------- §8.6c 对公收款账户（读运营选项） ----------------

type QydionCorporateAccount struct {
	AccountName string `json:"account_name"` // 户名
	AccountNo   string `json:"account_no"`   // 账号
	BankName    string `json:"bank_name"`    // 开户行
	Note        string `json:"note"`
}

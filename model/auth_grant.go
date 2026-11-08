package model

// 一次性授权码（auth grant）：跨域免登（purpose=web_console）与小程序统一 UID
// 绑定（purpose=mp_bind）共用的安全前置件。
// 设计依据：《多项目代码审计与统一账号方案-2026-11-06.md》第 7/8 节（方案 B）。
// 安全硬指标：随机 128bit、服务端只存 HMAC、短 TTL、单次消费、绑定 purpose/
// 接收方/创建者用户；消费走事务内原子 UPDATE，重放/过期/跨 purpose/跨接收方
// 都会以独立错误类型被拒绝（对外统一错误，审计里区分原因）。

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"gorm.io/gorm"
)

const (
	AuthGrantPurposeWebConsole = "web_console"
	AuthGrantPurposeMPBind     = "mp_bind"

	// AuthGrantCodeBytes 16 字节 = 128bit 随机码，URL-safe Base64 后 22 字符。
	AuthGrantCodeBytes = 16
	// AuthGrantDefaultTTLSeconds / min / max：默认 60 秒，钳制区间 [10,300]。
	AuthGrantDefaultTTLSeconds = 60
	AuthGrantMinTTLSeconds     = 10
	AuthGrantMaxTTLSeconds     = 300
	// AuthGrantCleanupRetention 已消费/已过期的码行保留时长，过期后由既有
	// 清理任务（service.StartAuthArtifactCleanup）兜底删除。
	AuthGrantCleanupRetention = 24 * time.Hour
)

var (
	ErrAuthGrantInvalid  = errors.New("auth grant is invalid")
	ErrAuthGrantExpired  = errors.New("auth grant has expired")
	ErrAuthGrantConsumed = errors.New("auth grant has already been consumed")
	// ErrAuthGrantMismatch 表示码存在但 purpose 或接收方与消费请求不符。
	ErrAuthGrantMismatch = errors.New("auth grant purpose or audience does not match")
)

// AuthGrant 码表：只存 HMAC(CodeHash)，明文码只在创建响应里出现一次。
type AuthGrant struct {
	Id         int64      `json:"id" gorm:"primaryKey"`
	CodeHash   string     `json:"-" gorm:"type:char(64);not null;uniqueIndex"`
	Purpose    string     `json:"purpose" gorm:"type:varchar(32);not null;index:idx_auth_grant_purpose_expiry"`
	Audience   string     `json:"audience" gorm:"type:varchar(64);not null"`
	UserId     int        `json:"user_id" gorm:"not null;index"`
	CreatedAt  time.Time  `json:"created_at"`
	ExpiresAt  time.Time  `json:"expires_at" gorm:"not null;index:idx_auth_grant_purpose_expiry"`
	ConsumedAt *time.Time `json:"consumed_at,omitempty" gorm:"index"`
}

func (AuthGrant) TableName() string {
	return "auth_grants"
}

func authGrantTokenHash(token string) string {
	// 与 AuthFlow 的域分离 HMAC 同构，但域前缀不同，两个码空间互不可换算。
	return common.GenerateHMACWithKey([]byte("auth-grant-v1:"+common.SessionSecret), token)
}

// ClampAuthGrantTTL 把配置的 TTL 钳制到安全区间，缺省回落到 60 秒。
func ClampAuthGrantTTL(seconds int) time.Duration {
	if seconds <= 0 {
		seconds = AuthGrantDefaultTTLSeconds
	}
	if seconds < AuthGrantMinTTLSeconds {
		seconds = AuthGrantMinTTLSeconds
	}
	if seconds > AuthGrantMaxTTLSeconds {
		seconds = AuthGrantMaxTTLSeconds
	}
	return time.Duration(seconds) * time.Second
}

// CreateAuthGrantCode 生成并落库一枚一次性授权码，返回明文码与码行。
// 调用方（controller）负责 purpose/接收方白名单与创建者校验。
func CreateAuthGrantCode(purpose, audience string, userID int, ttl time.Duration) (string, *AuthGrant, error) {
	purpose = strings.TrimSpace(purpose)
	audience = strings.TrimSpace(audience)
	if purpose == "" || audience == "" || userID <= 0 || ttl <= 0 {
		return "", nil, ErrAuthGrantInvalid
	}
	random := make([]byte, AuthGrantCodeBytes)
	if _, err := rand.Read(random); err != nil {
		return "", nil, fmt.Errorf("generate auth grant code: %w", err)
	}
	token := base64.RawURLEncoding.EncodeToString(random)
	now := time.Now()
	grant := &AuthGrant{
		CodeHash:  authGrantTokenHash(token),
		Purpose:   purpose,
		Audience:  audience,
		UserId:    userID,
		CreatedAt: now,
		ExpiresAt: now.Add(ttl),
	}
	if err := DB.Create(grant).Error; err != nil {
		return "", nil, err
	}
	return token, grant, nil
}

// ConsumeAuthGrant 事务内原子消费一枚授权码：只有命中 hash+purpose+audience
// 且未消费、未过期的行会被 UPDATE 占用，并发重放天然互斥（与 ConsumeAuthFlow
// 同一模式；SQLite 的 immediate 事务与唯一索引保证三端数据库行为一致）。
// 拒绝原因按 码无效→错配→已消费→已过期 的顺序判定，供审计区分，不外泄。
func ConsumeAuthGrant(token, purpose, audience string) (*AuthGrant, error) {
	token = strings.TrimSpace(token)
	purpose = strings.TrimSpace(purpose)
	audience = strings.TrimSpace(audience)
	if token == "" || purpose == "" || audience == "" {
		return nil, ErrAuthGrantInvalid
	}
	codeHash := authGrantTokenHash(token)
	now := time.Now()
	var consumed AuthGrant
	err := DB.Transaction(func(tx *gorm.DB) error {
		result := tx.Model(&AuthGrant{}).
			Where("code_hash = ? AND purpose = ? AND audience = ? AND consumed_at IS NULL AND expires_at > ?", codeHash, purpose, audience, now).
			Update("consumed_at", now)
		if result.Error != nil {
			return result.Error
		}
		var row AuthGrant
		readErr := tx.Where("code_hash = ?", codeHash).First(&row).Error
		if readErr != nil {
			if errors.Is(readErr, gorm.ErrRecordNotFound) {
				return ErrAuthGrantInvalid
			}
			return readErr
		}
		if result.RowsAffected != 1 {
			switch {
			case row.Purpose != purpose || row.Audience != audience:
				return ErrAuthGrantMismatch
			case row.ConsumedAt != nil:
				return ErrAuthGrantConsumed
			case !row.ExpiresAt.After(now):
				return ErrAuthGrantExpired
			default:
				return ErrAuthGrantInvalid
			}
		}
		consumed = row
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &consumed, nil
}

// DeleteExpiredAuthGrants 清理过期与已消费超过保留期的码行，挂在既有
// 认证工件清理任务里周期执行；消费路径本身的过期判定即惰性删除兜底。
func DeleteExpiredAuthGrants(now time.Time) error {
	cutoff := now.Add(-AuthGrantCleanupRetention)
	return DB.Where("expires_at < ? OR (consumed_at IS NOT NULL AND consumed_at < ?)", cutoff, cutoff).
		Delete(&AuthGrant{}).Error
}

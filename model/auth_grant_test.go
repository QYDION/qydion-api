package model

// 一次性授权码模型层测试：TTL 钳制、消费语义（无效/过期/重放/跨 purpose/
// 跨接收方）与过期清理。controller 层端到端测试见 controller/auth_grant_test.go。

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func setupAuthGrantModelTest(t *testing.T) {
	t.Helper()
	require.NoError(t, DB.AutoMigrate(&AuthGrant{}))
	t.Cleanup(func() {
		DB.Exec("DELETE FROM auth_grants")
	})
}

func createTestAuthGrant(t *testing.T, purpose, audience string, ttl time.Duration) (string, *AuthGrant) {
	t.Helper()
	token, grant, err := CreateAuthGrantCode(purpose, audience, 42, ttl)
	require.NoError(t, err)
	require.NotEmpty(t, token)
	// 只存哈希：明文码绝不落库。
	require.NotContains(t, grant.CodeHash, token)
	require.Len(t, token, 22) // 128bit → base64url 22 字符
	return token, grant
}

func TestAuthGrantTTLCIsClamped(t *testing.T) {
	assert.Equal(t, 60*time.Second, ClampAuthGrantTTL(0))
	assert.Equal(t, AuthGrantMinTTLSeconds*time.Second, ClampAuthGrantTTL(3))
	assert.Equal(t, 60*time.Second, ClampAuthGrantTTL(60))
	assert.Equal(t, AuthGrantMaxTTLSeconds*time.Second, ClampAuthGrantTTL(3600))
}

func TestAuthGrantIsBoundAndConsumedOnce(t *testing.T) {
	setupAuthGrantModelTest(t)
	token, created := createTestAuthGrant(t, AuthGrantPurposeWebConsole, "official_site", time.Minute)
	require.EqualValues(t, 42, created.UserId)
	assert.Nil(t, created.ConsumedAt)

	// 跨接收方：audience 与创建时不符即拒。
	_, err := ConsumeAuthGrant(token, AuthGrantPurposeWebConsole, "official_site2")
	assert.ErrorIs(t, err, ErrAuthGrantMismatch)
	// 跨 purpose：码存在但 purpose 不符即拒。
	_, err = ConsumeAuthGrant(token, AuthGrantPurposeMPBind, "official_site")
	assert.ErrorIs(t, err, ErrAuthGrantMismatch)
	// 未知码：拒绝且不泄露区分。
	_, err = ConsumeAuthGrant("not-a-real-code", AuthGrantPurposeWebConsole, "official_site")
	assert.ErrorIs(t, err, ErrAuthGrantInvalid)

	consumed, err := ConsumeAuthGrant(token, AuthGrantPurposeWebConsole, "official_site")
	require.NoError(t, err)
	require.NotNil(t, consumed.ConsumedAt)

	// 重放：同一码第二次消费必拒。
	_, err = ConsumeAuthGrant(token, AuthGrantPurposeWebConsole, "official_site")
	assert.ErrorIs(t, err, ErrAuthGrantConsumed)
	// 错配的重放尝试也不改变消费状态。
	_, err = ConsumeAuthGrant(token, AuthGrantPurposeMPBind, "official_site")
	assert.ErrorIs(t, err, ErrAuthGrantMismatch)
}

func TestAuthGrantExpiryIsEnforcedAndCleaned(t *testing.T) {
	setupAuthGrantModelTest(t)
	token, grant := createTestAuthGrant(t, AuthGrantPurposeMPBind, "mp_card", time.Minute)
	require.NoError(t, DB.Model(&AuthGrant{}).Where("id = ?", grant.Id).
		Update("expires_at", time.Now().Add(-2*AuthGrantCleanupRetention)).Error)

	_, err := ConsumeAuthGrant(token, AuthGrantPurposeMPBind, "mp_card")
	assert.ErrorIs(t, err, ErrAuthGrantExpired)

	// 清理只删过期/已消费超保留期的行；新鲜行保留。
	require.NoError(t, DeleteExpiredAuthGrants(time.Now()))
	var count int64
	DB.Model(&AuthGrant{}).Where("id = ?", grant.Id).Count(&count)
	assert.EqualValues(t, 0, count)

	freshToken, _ := createTestAuthGrant(t, AuthGrantPurposeMPBind, "mp_card", time.Minute)
	consumed, err := ConsumeAuthGrant(freshToken, AuthGrantPurposeMPBind, "mp_card")
	require.NoError(t, err)
	require.NoError(t, DeleteExpiredAuthGrants(time.Now()))
	DB.Model(&AuthGrant{}).Where("id = ?", consumed.Id).Count(&count)
	assert.EqualValues(t, 1, count, "刚消费的行在保留期内不应被清理")
}

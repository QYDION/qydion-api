package controller

// 一次性授权码端点测试（仿 controller/auth_flow_test.go 的进程内夹具模式）：
// 覆盖正常交换、重放拒绝、过期拒绝、跨 purpose 拒绝、跨接收方拒绝、频控生效、
// 审计有记录（且不含码明文）。模型层消费语义见 model/auth_grant_test.go。

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"maps"
	"strings"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/setting/system_setting"
	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

type authGrantTestEnv struct {
	user *model.User
}

// setupAuthGrantTest 建立独立 SQLite（主库/审计库分离）、一个启用用户，
// 并把 auth_grant 白名单配置快照为测试专用值，测试结束后恢复。
func setupAuthGrantTest(t *testing.T) *authGrantTestEnv {
	t.Helper()
	gin.SetMode(gin.TestMode)
	previousDB, previousLogDB := model.DB, model.LOG_DB
	previousMain, previousLog := common.MainDatabaseType(), common.LogDatabaseType()
	previousRedis, previousSecret := common.RedisEnabled, common.SessionSecret

	db, err := gorm.Open(sqlite.Open(t.TempDir()+"/auth-grant.db?_pragma=busy_timeout(30000)&_pragma=journal_mode(WAL)&_txlock=immediate"), &gorm.Config{})
	require.NoError(t, err)
	closePool(t, db)
	logDB, err := gorm.Open(sqlite.Open(t.TempDir()+"/auth-grant-log.db?_pragma=busy_timeout(30000)&_pragma=journal_mode(WAL)&_txlock=immediate"), &gorm.Config{})
	require.NoError(t, err)
	closePool(t, logDB)
	db.Logger = logger.Default.LogMode(logger.Silent)
	logDB.Logger = logger.Default.LogMode(logger.Silent)

	require.NoError(t, db.AutoMigrate(&model.User{}, &model.UserSession{}, &model.AuthGrant{}, &model.Option{}, &model.UserAccessToken{}))
	require.NoError(t, logDB.AutoMigrate(&model.AuditLog{}))
	model.DB, model.LOG_DB = db, logDB
	common.SetDatabaseTypes(common.DatabaseTypeSQLite, common.DatabaseTypeSQLite)
	common.RedisEnabled = false
	common.SessionSecret = "auth-grant-test-secret"

	// 白名单配置快照与恢复：GetAuthGrantSettings 返回包级指针，改动必须归还。
	settings := system_setting.GetAuthGrantSettings()
	savedSettings := *settings
	savedSettings.PurposeAudiences = maps.Clone(settings.PurposeAudiences)
	savedSettings.Audiences = maps.Clone(settings.Audiences)
	settings.TTLSeconds = 60
	settings.AttemptLimitPerMinute = 30
	settings.PurposeAudiences = map[string][]string{
		model.AuthGrantPurposeWebConsole: {"official_site", "official_site2"},
		model.AuthGrantPurposeMPBind:     {"mp_card", "mp_recipes"},
	}
	// mp_card 也配了 RedirectURL，仅为让「mp_bind 码打到 exchange 端点」能在
	// 消费阶段命中 purpose 错配（否则会先被接收方未配置拦截，测不到模型层）。
	settings.Audiences = map[string]system_setting.AuthGrantAudience{
		"official_site":  {RedirectURL: "https://site.example/login/bridge"},
		"official_site2": {RedirectURL: "https://site2.example/bridge"},
		"mp_card":        {RedirectURL: "https://mpcard.example/bridge"},
		"mp_recipes":     {},
	}
	t.Cleanup(func() {
		*settings = savedSettings
		model.DB, model.LOG_DB = previousDB, previousLogDB
		common.SetDatabaseTypes(previousMain, previousLog)
		common.RedisEnabled, common.SessionSecret = previousRedis, previousSecret
	})

	password, err := common.Password2Hash("grant-password")
	require.NoError(t, err)
	user := &model.User{Username: "grant-owner", Password: password, Role: common.RoleCommonUser, Status: common.UserStatusEnabled, Group: "default", AuthVersion: 1}
	require.NoError(t, db.Create(user).Error)

	resetAuthGrantAttemptLimiterForTest()
	return &authGrantTestEnv{user: user}
}

// newAuthGrantRouter 挂三个端点。identity 非 nil 时按 UserAuth 落上下文的
// 方式注入 id/role（与 securityEnrollmentRequest 的上下文注入同构）。
func (e *authGrantTestEnv) newAuthGrantRouter() *gin.Engine {
	router := gin.New()
	owner := e.user
	router.POST("/api/user/auth_grant", func(c *gin.Context) {
		c.Set("id", owner.Id)
		c.Set("role", owner.Role)
		c.Next()
	}, CreateAuthGrant)
	router.GET("/api/user/auth_grant/exchange", ExchangeAuthGrant)
	router.POST("/api/user/auth_grant/verify", VerifyAuthGrant)
	return router
}

func (e *authGrantTestEnv) doJSON(router *gin.Engine, method, path, body string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	if body != "" {
		request.Header.Set("Content-Type", "application/json")
	}
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	return response
}

// createAuthGrant 走真实 POST /api/user/auth_grant 创建一枚码并返回明文。
func (e *authGrantTestEnv) createAuthGrant(t *testing.T, router *gin.Engine, purpose, audience string) string {
	t.Helper()
	response := e.doJSON(router, "POST", "/api/user/auth_grant", fmt.Sprintf(`{"purpose":%q,"audience":%q}`, purpose, audience))
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	var result struct {
		Success bool `json:"success"`
		Data    struct {
			Code string `json:"code"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &result))
	require.True(t, result.Success, response.Body.String())
	require.NotEmpty(t, result.Data.Code)
	return result.Data.Code
}

func (e *authGrantTestEnv) auditRows(t *testing.T) []model.AuditLog {
	t.Helper()
	var rows []model.AuditLog
	require.NoError(t, model.LOG_DB.Where("action LIKE ?", "auth_grant%").Order("id ASC").Find(&rows).Error)
	return rows
}

// auditStringParam 取审计参数字符串值。AuditFields 反序列化时故意保留
// json.RawMessage（防整型精度丢失），所以回读后的值需再解一层引号。
func auditStringParam(params model.AuditFields, key string) (string, bool) {
	raw, ok := params[key]
	if !ok {
		return "", false
	}
	switch value := raw.(type) {
	case string:
		return value, true
	case json.RawMessage:
		var decoded string
		if err := json.Unmarshal(value, &decoded); err != nil {
			return "", false
		}
		return decoded, true
	default:
		return "", false
	}
}

func auditReasons(rows []model.AuditLog, action string) []string {
	reasons := make([]string, 0, len(rows))
	for _, row := range rows {
		if row.Action != action || row.Other.Op == nil {
			continue
		}
		if reason, ok := auditStringParam(row.Other.Op.Params, "reason"); ok {
			reasons = append(reasons, reason)
		}
	}
	return reasons
}

func TestAuthGrantCreateRequiresLogin(t *testing.T) {
	env := setupAuthGrantTest(t)
	router := gin.New()
	router.POST("/api/user/auth_grant", CreateAuthGrant)
	response := env.doJSON(router, "POST", "/api/user/auth_grant", `{"purpose":"web_console","audience":"official_site"}`)
	assert.Equal(t, http.StatusOK, response.Code)
	var result struct {
		Success bool `json:"success"`
	}
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &result))
	assert.False(t, result.Success)
}

func TestAuthGrantCreateRejectsUnknownPurposeOrAudience(t *testing.T) {
	env := setupAuthGrantTest(t)
	router := env.newAuthGrantRouter()
	for _, body := range []string{
		`{"purpose":"unknown","audience":"official_site"}`,
		`{"purpose":"web_console","audience":"no_such_audience"}`,
		// mp_bind 不允许 web_console 的接收方。
		`{"purpose":"mp_bind","audience":"official_site"}`,
		`{}`,
	} {
		response := env.doJSON(router, "POST", "/api/user/auth_grant", body)
		var result struct {
			Success bool `json:"success"`
		}
		require.NoError(t, json.Unmarshal(response.Body.Bytes(), &result), body)
		assert.False(t, result.Success, body)
	}
	var count int64
	require.NoError(t, model.DB.Model(&model.AuthGrant{}).Count(&count).Error)
	assert.EqualValues(t, 0, count, "非法创建不得产生码行")
}

func TestAuthGrantExchangeCreatesSessionAndRedirects(t *testing.T) {
	env := setupAuthGrantTest(t)
	router := env.newAuthGrantRouter()
	code := env.createAuthGrant(t, router, model.AuthGrantPurposeWebConsole, "official_site")

	response := env.doJSON(router, "GET", "/api/user/auth_grant/exchange?code="+code+"&audience=official_site", "")
	assert.Equal(t, http.StatusFound, response.Code, response.Body.String())
	assert.Equal(t, "https://site.example/login/bridge", response.Header().Get("Location"))
	cookies := strings.Join(response.Header().Values("Set-Cookie"), "\n")
	assert.Contains(t, cookies, "new_api_refresh=", "交换必须落下网关会话 Cookie")
	assert.Contains(t, cookies, "new_api_has_session=1")
	assert.NotContains(t, response.Body.String(), "access_token", "重定向响应不得夹带访问令牌")

	var session model.UserSession
	require.NoError(t, model.DB.Where("user_id = ?", env.user.Id).First(&session).Error)
	assert.Equal(t, "auth_grant", session.LoginMethod)

	rows := env.auditRows(t)
	assert.Contains(t, auditReasons(rows, "auth_grant_create"), "created")
	assert.Contains(t, auditReasons(rows, "auth_grant_consume"), "exchanged")
}

func TestAuthGrantVerifyReturnsUIDOnce(t *testing.T) {
	env := setupAuthGrantTest(t)
	router := env.newAuthGrantRouter()
	code := env.createAuthGrant(t, router, model.AuthGrantPurposeMPBind, "mp_card")

	response := env.doJSON(router, "POST", "/api/user/auth_grant/verify", fmt.Sprintf(`{"code":%q,"audience":"mp_card"}`, code))
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	var result struct {
		Success bool `json:"success"`
		Data    struct {
			UID int `json:"uid"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &result))
	require.True(t, result.Success, response.Body.String())
	assert.Equal(t, env.user.Id, result.Data.UID, "verify 必须返回创建者的统一 UID")

	// 重放拒绝，且不给小程序服务端产生任何会话。
	replay := env.doJSON(router, "POST", "/api/user/auth_grant/verify", fmt.Sprintf(`{"code":%q,"audience":"mp_card"}`, code))
	assert.Equal(t, http.StatusUnauthorized, replay.Code)
	assert.False(t, strings.Contains(replay.Body.String(), `"uid"`))
	var sessionCount int64
	require.NoError(t, model.DB.Model(&model.UserSession{}).Where("user_id = ?", env.user.Id).Count(&sessionCount).Error)
	assert.EqualValues(t, 0, sessionCount)
}

func TestAuthGrantExchangeRejectsReplay(t *testing.T) {
	env := setupAuthGrantTest(t)
	router := env.newAuthGrantRouter()
	code := env.createAuthGrant(t, router, model.AuthGrantPurposeWebConsole, "official_site")
	first := env.doJSON(router, "GET", "/api/user/auth_grant/exchange?code="+code+"&audience=official_site", "")
	require.Equal(t, http.StatusFound, first.Code)

	replay := env.doJSON(router, "GET", "/api/user/auth_grant/exchange?code="+code+"&audience=official_site", "")
	assert.Equal(t, http.StatusUnauthorized, replay.Code)
	assert.Contains(t, replay.Body.String(), authGrantRejectedMessage, "拒绝必须走统一错误，不泄露区分细节")
	var sessionCount int64
	require.NoError(t, model.DB.Model(&model.UserSession{}).Where("user_id = ?", env.user.Id).Count(&sessionCount).Error)
	assert.EqualValues(t, 1, sessionCount, "重放不得再建会话")
	assert.Contains(t, auditReasons(env.auditRows(t), "auth_grant_reject"), "replayed")
}

func TestAuthGrantExchangeRejectsExpired(t *testing.T) {
	env := setupAuthGrantTest(t)
	router := env.newAuthGrantRouter()
	code := env.createAuthGrant(t, router, model.AuthGrantPurposeWebConsole, "official_site")
	var grant model.AuthGrant
	require.NoError(t, model.DB.First(&grant).Error)
	require.NoError(t, model.DB.Model(&grant).Update("expires_at", time.Now().Add(-time.Second)).Error)

	response := env.doJSON(router, "GET", "/api/user/auth_grant/exchange?code="+code+"&audience=official_site", "")
	assert.Equal(t, http.StatusUnauthorized, response.Code)
	assert.Contains(t, response.Body.String(), authGrantRejectedMessage)
	assert.Contains(t, auditReasons(env.auditRows(t), "auth_grant_reject"), "expired")
}

func TestAuthGrantRejectsCrossPurpose(t *testing.T) {
	env := setupAuthGrantTest(t)
	router := env.newAuthGrantRouter()

	// web_console 码打到 verify 端点：official_site 不在 mp_bind 白名单里，
	// 消费前即拒，码不被烧掉。
	webCode := env.createAuthGrant(t, router, model.AuthGrantPurposeWebConsole, "official_site")
	verify := env.doJSON(router, "POST", "/api/user/auth_grant/verify", fmt.Sprintf(`{"code":%q,"audience":"official_site"}`, webCode))
	assert.Equal(t, http.StatusUnauthorized, verify.Code)
	exchange := env.doJSON(router, "GET", "/api/user/auth_grant/exchange?code="+webCode+"&audience=official_site", "")
	assert.Equal(t, http.StatusFound, exchange.Code, "被错 purpose 拒绝的码不应被烧掉")

	// mp_bind 码打到 exchange 端点：接收方配置齐全，会在消费阶段命中 purpose 错配。
	mpCode := env.createAuthGrant(t, router, model.AuthGrantPurposeMPBind, "mp_card")
	wrongExchange := env.doJSON(router, "GET", "/api/user/auth_grant/exchange?code="+mpCode+"&audience=mp_card", "")
	assert.Equal(t, http.StatusUnauthorized, wrongExchange.Code)
	// 错 purpose 的交换不烧码：verify 仍可用。
	verifyOK := env.doJSON(router, "POST", "/api/user/auth_grant/verify", fmt.Sprintf(`{"code":%q,"audience":"mp_card"}`, mpCode))
	require.Equal(t, http.StatusOK, verifyOK.Code)
	assert.Contains(t, auditReasons(env.auditRows(t), "auth_grant_reject"), "purpose_or_audience_mismatch")
}

func TestAuthGrantRejectsCrossReceiver(t *testing.T) {
	env := setupAuthGrantTest(t)
	router := env.newAuthGrantRouter()
	code := env.createAuthGrant(t, router, model.AuthGrantPurposeWebConsole, "official_site")

	// 接收方声明与创建时不符：官方站 A 的码不能由官方站 B 消费。
	wrongReceiver := env.doJSON(router, "GET", "/api/user/auth_grant/exchange?code="+code+"&audience=official_site2", "")
	assert.Equal(t, http.StatusUnauthorized, wrongReceiver.Code)
	assert.Contains(t, wrongReceiver.Body.String(), authGrantRejectedMessage)
	assert.Equal(t, "", wrongReceiver.Header().Get("Location"), "拒绝时不得重定向")

	// 码未被烧掉：正确接收方仍可交换。
	okReceiver := env.doJSON(router, "GET", "/api/user/auth_grant/exchange?code="+code+"&audience=official_site", "")
	assert.Equal(t, http.StatusFound, okReceiver.Code)
	assert.Contains(t, auditReasons(env.auditRows(t), "auth_grant_reject"), "purpose_or_audience_mismatch")
}

func TestAuthGrantRateLimit(t *testing.T) {
	env := setupAuthGrantTest(t)
	router := env.newAuthGrantRouter()
	settings := system_setting.GetAuthGrantSettings()
	settings.AttemptLimitPerMinute = 2
	t.Cleanup(func() { settings.AttemptLimitPerMinute = 30 })
	resetAuthGrantAttemptLimiterForTest()

	// 两次尝试（无效码）以内按统一错误处理，第三次起 429。
	for attempt := range 2 {
		response := env.doJSON(router, "GET", "/api/user/auth_grant/exchange?code=bad-code&audience=official_site", "")
		assert.Equal(t, http.StatusUnauthorized, response.Code, fmt.Sprintf("attempt %d", attempt))
	}
	blocked := env.doJSON(router, "GET", "/api/user/auth_grant/exchange?code=bad-code&audience=official_site", "")
	assert.Equal(t, http.StatusTooManyRequests, blocked.Code)
	assert.Contains(t, auditReasons(env.auditRows(t), "auth_grant_reject"), "rate_limited")
}

func TestAuthGrantAuditNeverContainsCode(t *testing.T) {
	env := setupAuthGrantTest(t)
	router := env.newAuthGrantRouter()
	code := env.createAuthGrant(t, router, model.AuthGrantPurposeWebConsole, "official_site")
	require.Equal(t, http.StatusFound, env.doJSON(router, "GET", "/api/user/auth_grant/exchange?code="+code+"&audience=official_site", "").Code)
	require.Equal(t, http.StatusUnauthorized, env.doJSON(router, "GET", "/api/user/auth_grant/exchange?code="+code+"&audience=official_site", "").Code)

	rows := env.auditRows(t)
	require.NotEmpty(t, rows)
	actions := make([]string, 0, len(rows))
	for _, row := range rows {
		actions = append(actions, row.Action)
		// 审计三件套：Content、Other 结构体、整行 JSON 都不得出现码明文。
		other, err := json.Marshal(row.Other)
		require.NoError(t, err)
		assert.NotContains(t, row.Content, code)
		assert.NotContains(t, string(other), code)
	}
	assert.Contains(t, actions, "auth_grant_create")
	assert.Contains(t, actions, "auth_grant_consume")
	assert.Contains(t, actions, "auth_grant_reject")
}

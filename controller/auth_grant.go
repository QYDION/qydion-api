package controller

// 一次性授权码（auth grant）端点：跨域免登（web_console）与小程序统一 UID
// 绑定（mp_bind）共用的安全前置件。
// 设计依据：《多项目代码审计与统一账号方案-2026-11-06.md》第 7/8 节（方案 B）。
//
// 三个端点：
//   POST /api/user/auth_grant          需登录：创建授权码（明文只在本次响应出现一次）
//   GET  /api/user/auth_grant/exchange 未认证可达：码换网关会话（Cookie）并重定向接收方
//   POST /api/user/auth_grant/verify   服务端对服务端：码换 {uid}，供小程序落映射
//
// 安全硬指标：随机 128bit、服务端只存 HMAC、TTL 默认 60 秒（钳制 [10,300]）、
// 单次消费、绑定 purpose + 接收方（配置白名单，fail closed）+ 创建者用户；
// 交换/校验失败一律统一错误不泄露细节；全流程审计（创建/消费/拒绝），审计与
// 日志不含码明文与码哈希。

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/logger"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting/system_setting"
	"github.com/gin-gonic/gin"
)

// authGrantRejectedMessage 是所有码相关失败的统一对外错误：不区分无效/过期/
// 已消费/错配，避免错误预言机。失败原因只进审计。
const authGrantRejectedMessage = "authorization code is invalid or expired"

// authGrantAttemptLimiter 是交换/校验端点的每 IP 基本频控（进程内滑动窗口）。
// 多实例部署时各实例独立计数，作为 route 级 CriticalRateLimit 之外的第二道
// 限速；超出即 429。测试通过 resetAuthGrantAttemptLimiterForTest 重置。
var authGrantAttemptLimiter common.InMemoryRateLimiter

func init() {
	authGrantAttemptLimiter.Init(30 * time.Minute)
}

// resetAuthGrantAttemptLimiterForTest 仅供同包测试重建限流器，清空计数。
func resetAuthGrantAttemptLimiterForTest() {
	authGrantAttemptLimiter = common.InMemoryRateLimiter{}
	authGrantAttemptLimiter.Init(30 * time.Minute)
}

func authGrantAttemptAllowed(c *gin.Context, mark string) bool {
	limit := system_setting.GetAuthGrantSettings().AttemptLimitPerMinute
	if limit <= 0 {
		return true
	}
	return authGrantAttemptLimiter.Request("AGX:"+mark+":"+c.ClientIP(), limit, 60)
}

// authGrantAudienceAllowed 判定接收方是否在白名单内且被该 purpose 允许。
// purpose 列表缺失或为空一律拒绝（fail closed），不做"空即全放行"。
func authGrantAudienceAllowed(purpose, audience string) bool {
	settings := system_setting.GetAuthGrantSettings()
	if _, ok := settings.Audiences[audience]; !ok {
		return false
	}
	for _, allowed := range settings.PurposeAudiences[purpose] {
		if allowed == audience {
			return true
		}
	}
	return false
}

// authGrantRedirectURL 读取接收方的重定向地址并做封闭校验：只接受 http(s)
// 绝对地址、无用户信息、无控制字符；重定向目标只能来自管理员配置，
// 请求参数传什么都改变不了它（防开放重定向）。
func authGrantRedirectURL(audience string) (string, bool) {
	raw := strings.TrimSpace(system_setting.GetAuthGrantSettings().Audiences[audience].RedirectURL)
	if raw == "" || strings.ContainsAny(raw, "\r\n") {
		return "", false
	}
	parsed, err := url.Parse(raw)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") ||
		parsed.Host == "" || parsed.User != nil || parsed.Fragment != "" {
		return "", false
	}
	return parsed.String(), true
}

// recordAuthGrantAudit 记录一行审计：action ∈ create/consume/reject。
// grant 为 nil 时（码无效等，不知道创建者）不填用户身份，由 RecordAuditLog
// 归为未知角色（仅 root 可见）。params 只放 purpose/audience/grant_id/reason，
// 严禁放入码明文与码哈希。
func recordAuthGrantAudit(c *gin.Context, grant *model.AuthGrant, action, reason string, success bool) {
	userID, actorRole := 0, 0
	if grant != nil {
		userID = grant.UserId
		if creator, err := model.GetUserCache(grant.UserId); err == nil {
			actorRole = creator.Role
		}
	}
	params := model.AuditFields{}
	if grant != nil {
		params["purpose"] = grant.Purpose
		params["audience"] = grant.Audience
		params["grant_id"] = grant.Id
	}
	if reason != "" {
		params["reason"] = reason
	}
	entry := model.AuditLog{
		UserId:    userID,
		ActorRole: actorRole,
		Category:  model.AuditCategorySecurity,
		Action:    "auth_grant_" + action,
		Success:   success,
		Content:   fmt.Sprintf("one-time auth grant %s (%s)", action, reason),
		Other: model.AuditOther{
			Op: &model.AuditOperation{Action: "auth_grant_" + action, Params: params},
		},
	}
	model.RecordAuditLog(c, entry)
}

// authGrantReject 统一拒绝出口：审计一行 reject + 统一错误响应。
func authGrantReject(c *gin.Context, grant *model.AuthGrant, reason string) {
	recordAuthGrantAudit(c, grant, "reject", reason, false)
	common.ApiErrorStatus(c, http.StatusUnauthorized, common.NewMessage(authGrantRejectedMessage))
}

type authGrantCreateRequest struct {
	Purpose  string `json:"purpose"`
	Audience string `json:"audience"`
}

// CreateAuthGrant POST /api/user/auth_grant（需登录）：创建一次性授权码。
// 出参：{code, purpose, audience, expires_in, expires_at, grant_id}；
// code 明文只在本次响应出现，服务端此后只持有 HMAC。
func CreateAuthGrant(c *gin.Context) {
	userID := c.GetInt("id")
	if userID <= 0 {
		common.ApiErrorMsg(c, "Unauthenticated")
		return
	}
	var req authGrantCreateRequest
	if err := common.DecodeJson(c.Request.Body, &req); err != nil {
		common.ApiErrorMsg(c, "Invalid parameters")
		return
	}
	req.Purpose = strings.TrimSpace(req.Purpose)
	req.Audience = strings.TrimSpace(req.Audience)
	if req.Purpose != model.AuthGrantPurposeWebConsole && req.Purpose != model.AuthGrantPurposeMPBind {
		common.ApiErrorMsg(c, "Unsupported purpose")
		return
	}
	if !authGrantAudienceAllowed(req.Purpose, req.Audience) {
		common.ApiErrorMsg(c, "Receiver is not allowed for this purpose")
		return
	}
	ttl := model.ClampAuthGrantTTL(system_setting.GetAuthGrantSettings().TTLSeconds)
	token, grant, err := model.CreateAuthGrantCode(req.Purpose, req.Audience, userID, ttl)
	if err != nil {
		logger.LogError(c.Request.Context(), common.LogText("create auth grant failed (user_id=%d): %v", userID, err))
		common.ApiErrorMsg(c, "Database error, please contact the administrator")
		return
	}
	recordAuthGrantAudit(c, grant, "create", "created", true)
	setAuthNoStore(c)
	common.ApiSuccess(c, gin.H{
		"code":       token,
		"purpose":    grant.Purpose,
		"audience":   grant.Audience,
		"expires_in": int(ttl.Seconds()),
		"expires_at": grant.ExpiresAt.Unix(),
		"grant_id":   grant.Id,
	})
}

// ExchangeAuthGrant GET /api/user/auth_grant/exchange?code=&audience=
//（未认证可达）：web_console 码换网关会话。校验通过后为创建者创建全新登录
// 会话（Refresh Cookie 仅落在网关域），再重定向到接收方配置的地址。
// 失败一律统一错误；成功后码行立即失效（单次消费）。
func ExchangeAuthGrant(c *gin.Context) {
	if !authGrantAttemptAllowed(c, "exchange") {
		recordAuthGrantAudit(c, nil, "reject", "rate_limited", false)
		common.ApiErrorStatus(c, http.StatusTooManyRequests, common.NewMessage("too many requests"))
		return
	}
	code := strings.TrimSpace(c.Query("code"))
	audience := strings.TrimSpace(c.Query("audience"))
	if code == "" || audience == "" {
		authGrantReject(c, nil, "missing_params")
		return
	}
	// 接收方配置先于消费校验：接收方尚未配置时码不被烧掉，合法用户可重试。
	redirectURL, ok := authGrantRedirectURL(audience)
	if !ok {
		authGrantReject(c, nil, "receiver_not_configured")
		return
	}
	grant, err := model.ConsumeAuthGrant(code, model.AuthGrantPurposeWebConsole, audience)
	if err != nil {
		authGrantReject(c, nil, rejectReasonFor(err))
		return
	}
	// 会话签发失败时码已消费（单次消费语义），创建者重新生成即可。
	bundle, err := service.CreateLoginSession(grant.UserId, "auth_grant", c.ClientIP(), c.Request.UserAgent())
	if err != nil {
		logger.LogError(c.Request.Context(), common.LogText("auth grant exchange session issue failed (grant_id=%d): %v", grant.Id, err))
		authGrantReject(c, grant, "session_issue_failed")
		return
	}
	service.WriteRefreshCookie(c, bundle.RefreshToken)
	setAuthNoStore(c)
	recordAuthGrantAudit(c, grant, "consume", "exchanged", true)
	http.Redirect(c.Writer, c.Request, redirectURL, http.StatusFound)
}

type authGrantVerifyRequest struct {
	Code     string `json:"code"`
	Audience string `json:"audience"`
}

// VerifyAuthGrant POST /api/user/auth_grant/verify（服务端对服务端，body:
// {"code","audience"}）：mp_bind 码换 {uid}，供小程序服务端落统一 UID 映射。
// 同样一次性 + 短 TTL；失败一律统一错误。
func VerifyAuthGrant(c *gin.Context) {
	if !authGrantAttemptAllowed(c, "verify") {
		recordAuthGrantAudit(c, nil, "reject", "rate_limited", false)
		common.ApiErrorStatus(c, http.StatusTooManyRequests, common.NewMessage("too many requests"))
		return
	}
	var req authGrantVerifyRequest
	if err := common.DecodeJson(c.Request.Body, &req); err != nil {
		authGrantReject(c, nil, "missing_params")
		return
	}
	req.Code = strings.TrimSpace(req.Code)
	req.Audience = strings.TrimSpace(req.Audience)
	if req.Code == "" || req.Audience == "" || !authGrantAudienceAllowed(model.AuthGrantPurposeMPBind, req.Audience) {
		authGrantReject(c, nil, "missing_params_or_receiver_denied")
		return
	}
	grant, err := model.ConsumeAuthGrant(req.Code, model.AuthGrantPurposeMPBind, req.Audience)
	if err != nil {
		authGrantReject(c, nil, rejectReasonFor(err))
		return
	}
	recordAuthGrantAudit(c, grant, "consume", "verified", true)
	setAuthNoStore(c)
	common.ApiSuccess(c, gin.H{"uid": grant.UserId})
}

// rejectReasonFor 把模型层错误映射为审计用的拒绝原因；对外仍走统一错误。
func rejectReasonFor(err error) string {
	switch err {
	case model.ErrAuthGrantExpired:
		return "expired"
	case model.ErrAuthGrantConsumed:
		return "replayed"
	case model.ErrAuthGrantMismatch:
		return "purpose_or_audience_mismatch"
	default:
		return "invalid_code"
	}
}

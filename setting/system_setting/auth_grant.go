package system_setting

// 一次性授权码（auth grant）接收方白名单配置。
// 设计依据：《多项目代码审计与统一账号方案-2026-11-06.md》第 7/8 节（方案 B）。
// purpose 与码表模型见 model/auth_grant.go；本文件只负责接收方（audience）白名单
// 与全局参数，配置键前缀为 auth_grant.，管理员可通过 option API（PUT /api/option/，
// key 形如 auth_grant.audiences）更新，重启后经 loadOptionsFromDatabase 装载。

import (
	"github.com/QuantumNous/new-api/setting/config"
)

// AuthGrantAudience 描述一个接收方。web_console 交换成功后浏览器会被重定向到
// RedirectURL；mp_bind 的接收方只做身份声明，不使用 RedirectURL。
type AuthGrantAudience struct {
	// RedirectURL 为 http(s) 绝对地址，只能由管理员在配置里写死；
	// 交换端点绝不接受请求参数里的跳转地址，避免开放重定向。
	RedirectURL string `json:"redirect_url"`
}

// AuthGrantSettings 是 auth_grant 配置模块的默认值载体。
type AuthGrantSettings struct {
	// TTLSeconds 是授权码默认有效期（秒），读侧钳制在 [10,300]。
	TTLSeconds int `json:"ttl_seconds"`
	// PurposeAudiences 把 purpose 映射到允许的接收方 ID 列表。
	// 列表为空 = 该 purpose 停用（fail closed），不做"空即全放行"。
	PurposeAudiences map[string][]string `json:"purpose_audiences"`
	// Audiences 是接收方白名单本体：ID -> 配置。不在表内的接收方 ID 一律拒绝。
	Audiences map[string]AuthGrantAudience `json:"audiences"`
	// AttemptLimitPerMinute 是交换/校验端点每 IP 每 60 秒的尝试上限（含失败）。
	AttemptLimitPerMinute int `json:"attempt_limit_per_minute"`
}

// purpose 取值与 model.AuthGrantPurposeWebConsole / model.AuthGrantPurposeMPBind
// 保持一致（字符串复制而非包引用，避免 setting 包反向依赖 model 包）。
var defaultAuthGrantSettings = AuthGrantSettings{
	TTLSeconds: 60,
	PurposeAudiences: map[string][]string{
		"web_console": {"official_site"},
		"mp_bind":     {"mp_card", "mp_recipes"},
	},
	// 默认接收方只登记 ID 占位：web_console 对应国内官网，mp_bind 对应两个
	// 小程序（名片/该吃啥）。RedirectURL 默认为空，交换端点会统一拒绝并留审计，
	// 管理员必须显式配置真实跳转地址后 web_console 才可用。
	Audiences: map[string]AuthGrantAudience{
		"official_site": {},
		"mp_card":       {},
		"mp_recipes":    {},
	},
	AttemptLimitPerMinute: 30,
}

func init() {
	config.GlobalConfig.Register("auth_grant", &defaultAuthGrantSettings)
}

// GetAuthGrantSettings 返回配置对象的指针；读侧不得长期缓存该指针，
// 管理端更新配置后对象字段会被原位覆盖。
func GetAuthGrantSettings() *AuthGrantSettings {
	return &defaultAuthGrantSettings
}

package service

import "strings"

// kiroCatalogMode Kiro 模型目录门控的执行模式。
// off=完全关闭门控；shadow=只记录不拦截（观察模式）；enforce=拦截不在目录内的模型。
type kiroCatalogMode string

const (
	kiroCatalogModeOff     kiroCatalogMode = "off"
	kiroCatalogModeShadow  kiroCatalogMode = "shadow"
	kiroCatalogModeEnforce kiroCatalogMode = "enforce"
)

// kiroModelCatalogModeKey 账号级覆写在 Account.Extra 中的键。
// 空字符串表示继承平台默认（UI 总是发送该键，因为 UpdateExtra 是 JSONB merge，
// 省略键不会删除旧值）。
const kiroModelCatalogModeKey = "kiro_model_catalog_mode"

// parseKiroCatalogMode 解析模式字符串：trim + lowercase 后仅接受 off/shadow/enforce。
// 空串与任何未知值返回 ok=false（调用方按「未配置」处理）。
func parseKiroCatalogMode(raw string) (kiroCatalogMode, bool) {
	switch kiroCatalogMode(strings.ToLower(strings.TrimSpace(raw))) {
	case kiroCatalogModeOff:
		return kiroCatalogModeOff, true
	case kiroCatalogModeShadow:
		return kiroCatalogModeShadow, true
	case kiroCatalogModeEnforce:
		return kiroCatalogModeEnforce, true
	}
	return "", false
}

// kiroCatalogModeOverride 读取账号级模式覆写。nil 账号、Extra 缺失、
// 空串（继承）与非法值一律返回 ok=false。
func (a *Account) kiroCatalogModeOverride() (kiroCatalogMode, bool) {
	if a == nil {
		return "", false
	}
	return parseKiroCatalogMode(a.GetExtraString(kiroModelCatalogModeKey))
}

// KiroModelCatalogRuntime 平台级门控运行时配置（来自 settings）。
type KiroModelCatalogRuntime struct {
	Mode         kiroCatalogMode
	EmergencyOff bool
}

// resolveKiroCatalogMode 纯函数：紧急关闭 > 账号覆写 > 平台默认。
// 零值 rt.Mode（""）按 shadow 处理，保证手工构造的零值运行时也是安全姿态。
func resolveKiroCatalogMode(rt KiroModelCatalogRuntime, override *Account) kiroCatalogMode {
	if rt.EmergencyOff {
		return kiroCatalogModeOff
	}
	if mode, ok := override.kiroCatalogModeOverride(); ok {
		return mode
	}
	if rt.Mode == "" {
		return kiroCatalogModeShadow
	}
	return rt.Mode
}

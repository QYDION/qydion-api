package controller

// 后台缺口⑦ 死路径回归：估算占比统计在独立日志库（LOG_SQL_DSN）部署下必须查 LOG_DB。
// 历史 bug：句柄曾用主库 DB——分离部署下主库 logs 表恒空 → total 恒 0 → 提前 return，告警永不触发。
// 本测试用两条独立内存 SQLite 模拟「主库 / 日志库」分离：日志只写日志库，主库 logs 空表，
// 断言统计从日志库读出（total>0 即证明没用主库）、占比超阈时告警分支可达；
// 告警出口在收件人未配置时降级日志，全程零真实外发。

import (
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/service"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

const estimatedTestDate = "2026-01-15"

// setupEstimatedRatioDBs 搭建「主库 / 日志库」分离替身，返回日志库窗口起点。
func setupEstimatedRatioDBs(t *testing.T) (mainDB, logDB *gorm.DB, start int64) {
	t.Helper()
	prevDB, prevLog := model.DB, model.LOG_DB
	prevSMTPServer, prevSMTPAccount := common.SMTPServer, common.SMTPAccount
	// 邮件出口兜底降级：即便进程内残留过 SMTP 配置也不外发
	common.SMTPServer, common.SMTPAccount = "", ""
	service.QydionSettingsBustCache() // 清掉其他测试残留的运营键缓存（含 QydionAlertEmail）
	t.Cleanup(func() {
		model.DB, model.LOG_DB = prevDB, prevLog
		common.SMTPServer, common.SMTPAccount = prevSMTPServer, prevSMTPAccount
	})

	cfg := &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)}
	var err error
	mainDB, err = gorm.Open(sqlite.Open(":memory:"), cfg)
	require.NoError(t, err)
	require.NoError(t, mainDB.AutoMigrate(&model.Log{}, &model.Option{}))
	logDB, err = gorm.Open(sqlite.Open(":memory:"), cfg)
	require.NoError(t, err)
	require.NoError(t, logDB.AutoMigrate(&model.Log{}))
	model.DB, model.LOG_DB = mainDB, logDB

	shanghai := time.FixedZone("CST", 8*3600)
	day, err := time.ParseInLocation("2006-01-02", estimatedTestDate, shanghai)
	require.NoError(t, err)
	return mainDB, logDB, day.Unix()
}

func TestQydionCheckEstimatedRatioSeparateLogDB(t *testing.T) {
	mainDB, logDB, start := setupEstimatedRatioDBs(t)

	// 前置断言：主库 logs 空表——这正是旧实现 total 恒 0 的现场
	var mainCount int64
	require.NoError(t, mainDB.Model(&model.Log{}).Count(&mainCount).Error)
	require.Zero(t, mainCount, "主库应为空表（模拟分离部署）")

	// 日志库写入 100 条消费日志，其中 5 条带估算标记（占比 5% > 默认阈值 1%）
	for i := 0; i < 100; i++ {
		other := `{"model_name":"gpt-4o"}`
		if i < 5 {
			other = `{"model_name":"gpt-4o","local_count_tokens":true}`
		}
		row := model.Log{
			UserId: 1, Type: model.LogTypeConsume,
			CreatedAt: start + int64(i)*100, // 窗口内（start..start+86400）
			Other:     other,
			PromptTokens: 10, CompletionTokens: 20,
			ChannelId: 1, ModelName: "gpt-4o", Quota: 100,
		}
		require.NoError(t, logDB.Create(&row).Error)
	}

	total, estimated, ratio := qydionCheckEstimatedRatio(estimatedTestDate)
	assert.Equal(t, int64(100), total, "total 必须来自日志库（主库为空）")
	assert.Equal(t, int64(5), estimated, "估算标记条数")
	assert.InDelta(t, 5.0, ratio, 0.001, "占比 5%%，超过默认阈值 1%% → 告警分支可达")
}

func TestQydionCheckEstimatedRatioEmptyWindow(t *testing.T) {
	_, logDB, start := setupEstimatedRatioDBs(t)

	// 日志库存在数据但落在窗口之外：统计应为 0（验证窗口过滤，且 total=0 不触发告警）
	row := model.Log{
		UserId: 1, Type: model.LogTypeConsume,
		CreatedAt: start + 86400 + 3600, // 窗口结束之后
		Other:     `{"local_count_tokens":true}`,
		ChannelId: 1, ModelName: "gpt-4o",
	}
	require.NoError(t, logDB.Create(&row).Error)

	total, estimated, ratio := qydionCheckEstimatedRatio(estimatedTestDate)
	assert.Equal(t, int64(0), total)
	assert.Equal(t, int64(0), estimated)
	assert.Equal(t, float64(0), ratio)
}

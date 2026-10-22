package main

import (
	"reflect"
	"testing"
	"time"

	"github.com/wcpe/jianartifact/apps/server/internal/config"
	"github.com/wcpe/jianartifact/apps/server/internal/domain"
	"github.com/wcpe/jianartifact/apps/server/internal/persistence"
	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

// 本文件是**默认值漂移**的护栏。
//
// 同一批默认值在四个包里各有一份定义，而它们必须同值：
//
//	config.defaultSearchTimeout        ↔ domain.searchTimeoutDefault（repository_service.go）
//	config.defaultCapacityCacheTTL     ↔ repository.defaultCapacityCacheTTL（operations_observability_repo.go）
//	config.defaultSQLiteMaxConns       ↔ persistence.DefaultMaxOpenConns（db.go）
//
// 为什么必须有这道断言：这三组常量的关系**只有**注释在维持（「与 X 侧兜底常量保持同值」），
// 而两侧的测试各自写死本包的字面量——把其中一侧从 30s 改成 60s，两侧测试照样全绿，
// 生产却变成「装配层注入了 60s，未装配路径（测试构造、协议侧直连）走 30s」的静默分叉。
// 这类分叉不会报错，只会让排障时看到的数字与文档对不上。
//
// 为什么放在 cmd/jianartifact：它是唯一同时看得到 config 与 domain / repository / persistence
// 的包（装配层）。放进任何一个下层包都要么反向依赖、要么绕过分层约定。
//
// 三条配对各自的读取方式见下面各段的注释：domain 与 persistence 有可用的读法；
// repository 侧常量未导出且**没有**访问器，只能反射读结构体默认值，理由写在对应断言处。
func TestServiceDefaultsMatchConfigDefaults(t *testing.T) {
	t.Setenv(config.EnvDataDir, t.TempDir())
	// 显式给密钥：否则 config.Load 会走「生成并落盘随机密钥」的分支，测试会写文件。
	t.Setenv(config.EnvJWTSecret, "defaults-drift-test-secret-key-32b")
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("加载配置：%v", err)
	}

	// 一、搜索预算：config 注入值 vs domain 兜底常量。
	//
	// domain 的 searchTimeoutDefault 未导出，但 NewRepositoryService 会把默认值写进字段，
	// 而 SearchTimeout() 是给协议层/运维排查用的公开读取口，取它即可（服务只被读一个
	// 字段，不触碰任何依赖，故依赖参数给 nil）。
	svc := domain.NewRepositoryService(nil, nil, nil, nil, nil)
	if got := svc.SearchTimeout(); got != cfg.SearchTimeout {
		t.Fatalf("搜索预算默认值漂移：domain 兜底 %s，config 注入 %s", got, cfg.SearchTimeout)
	}

	// 二、SQLite 连接池上限：config 注入值 vs persistence 兜底常量（导出的，直接比）。
	if cfg.SQLiteMaxConns != persistence.DefaultMaxOpenConns {
		t.Fatalf("连接池上限默认值漂移：persistence 兜底 %d，config 注入 %d",
			persistence.DefaultMaxOpenConns, cfg.SQLiteMaxConns)
	}

	// 三、容量聚合缓存 TTL：config 注入值 vs repository 兜底常量。
	//
	// repository 的 defaultCapacityCacheTTL 未导出，且没有 getter，SetCapacityCacheTTL 只写
	// 不读——从 cmd 包唯一能拿到「未注入时的默认值」的方式就是反射读字段。这里刻意用反射
	// 而不是放弃断言：这三组里它是**最容易被单侧改动**的一个（30s 这种「拍出来的数字」），
	// 放弃它等于让本文件只保护两条，而风险最高的一条裸露。
	//
	// 代价是字段名被写死：改名时本测试会红，报错信息已指向该字段，改测试即可。
	repo := repository.NewOperationsObservabilityRepo(nil)
	field := reflect.ValueOf(repo).Elem().FieldByName("capacityTTL")
	if !field.IsValid() {
		t.Fatalf("读取 repository 侧的缓存 TTL 兜底值失败：OperationsObservabilityRepo 已无 capacityTTL 字段；" +
			"若字段被改名或改成有访问器，请同步更新本测试的读法")
	}
	repoDefault := time.Duration(field.Int())
	if repoDefault != cfg.CapacityCacheTTL {
		t.Fatalf("容量缓存 TTL 默认值漂移：repository 兜底 %s，config 注入 %s", repoDefault, cfg.CapacityCacheTTL)
	}
}

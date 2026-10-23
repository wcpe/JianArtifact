package main

import (
	"context"
	"time"

	"github.com/wcpe/jianartifact/apps/server/internal/domain"
	"github.com/wcpe/jianartifact/apps/server/internal/scheduler"
)

// registerUpstreamProbeJob 把「上游健康定时探测」注册为调度器作业（FR-159）。
//
// 间隔 <= 0 或探测函数为空时不注册（禁用语义，对齐 JIAN_UPSTREAM_PROBE_INTERVAL=0）。
// 作业名固定为 upstream-probe：它同时是维护作业清单（/admin/maintenance/jobs）里
// 手动触发该作业的名字，改名会破坏那条运维入口。
//
// 说明为什么探测作业不返回错误：单个上游不可达是**正常的观测结果**，不是作业失败。
// 把它记成失败会让 jobs 清单长期亮红灯，运维再也看不出「作业本身坏了」与
// 「上游真的挂了」这两件事的区别。真正的失败（列举仓库读库失败）仍如实上报。
func registerUpstreamProbeJob(sched *scheduler.Scheduler, interval time.Duration, probe func(context.Context) (domain.UpstreamProbeResult, error)) {
	if interval <= 0 || probe == nil {
		return
	}
	sched.Register("upstream-probe", interval, func(ctx context.Context) error {
		_, err := probe(ctx)
		return err
	})
}

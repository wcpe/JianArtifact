//go:build !linux && !windows

package domain

import (
	"time"

	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

type unsupportedHostCollector struct{ ready func() bool }

func NewHostCollector(_ string, ready func() bool) HostCollector {
	return &unsupportedHostCollector{ready: ready}
}
func clockTicksPerSecond() uint64 { return 1 }
func (c *unsupportedHostCollector) Collect(at time.Time) HostRawSample {
	readiness := repository.MetricStateOK
	code := ""
	if c.ready != nil && !c.ready() {
		readiness, code = repository.MetricStateError, "not_ready"
	}
	return HostRawSample{At: at, HostState: repository.MetricStateUnsupported, HostErrorCode: "platform_unsupported", NetworkState: repository.MetricStateUnsupported, NetworkErrorCode: "platform_unsupported", ProcessState: repository.MetricStateUnsupported, ProcessErrorCode: "platform_unsupported", ReadinessState: readiness, ReadinessErrorCode: code}
}

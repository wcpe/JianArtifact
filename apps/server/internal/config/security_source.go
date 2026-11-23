package config

import (
	"fmt"
	"os"

	"github.com/wcpe/jianartifact/apps/server/internal/auditctx"
)

// EnvTrustedProxyCIDRs 是可信连接代理网段的配置键；未设置时不信任转发头。
const EnvTrustedProxyCIDRs = "JIAN_TRUSTED_PROXY_CIDRS"

func loadSourcePolicy() (auditctx.Policy, error) {
	policy, err := auditctx.NewPolicy(os.Getenv(EnvTrustedProxyCIDRs))
	if err != nil {
		return auditctx.Policy{}, fmt.Errorf("%s 配置无效：%w", EnvTrustedProxyCIDRs, err)
	}
	return policy, nil
}

package api

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/wcpe/jianartifact/apps/server/internal/auth"
	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

// GetReplicationApplyLogs 返回复制接收审计记录，仅管理员可见。
// 支持 sourceNode/sourceSeq/peerURL/entityType/entityKey/op/result 筛选。
func (h *Handlers) GetReplicationApplyLogs(c *gin.Context, params GetReplicationApplyLogsParams) {
	if _, ok := requireAdmin(c); !ok {
		return
	}
	if h.replicationApplyLogs == nil {
		auth.WriteError(c, http.StatusConflict, "conflict", "复制接收审计存储未就绪")
		return
	}
	if params.Result != nil && !params.Result.Valid() {
		auth.WriteError(c, http.StatusBadRequest, "bad_request", "result 值无效")
		return
	}
	filter, valid := replicationApplyLogFilter(params)
	if !valid {
		auth.WriteError(c, http.StatusBadRequest, "bad_request", "sourceSeq 须为非负整数")
		return
	}
	items, err := h.replicationApplyLogs.List(filter)
	if err != nil {
		writeDomainErr(c, err)
		return
	}
	total, err := h.replicationApplyLogs.Count(filter)
	if err != nil {
		writeDomainErr(c, err)
		return
	}
	response := ReplicationApplyLogList{Items: make([]ReplicationApplyLog, 0, len(items)), Total: total}
	for _, item := range items {
		response.Items = append(response.Items, toAPIReplicationApplyLog(item))
	}
	c.JSON(http.StatusOK, response)
}

func replicationApplyLogFilter(params GetReplicationApplyLogsParams) (repository.ReplicationApplyLogFilter, bool) {
	filter := repository.ReplicationApplyLogFilter{Limit: 50, Offset: 0}
	if params.Limit != nil && *params.Limit > 0 && *params.Limit <= 200 {
		filter.Limit = *params.Limit
	}
	if params.Offset != nil && *params.Offset >= 0 {
		filter.Offset = *params.Offset
	}
	filter.SourceNode = stringValue(params.SourceNode)
	filter.PeerURL = stringValue(params.PeerURL)
	filter.EntityType = stringValue(params.EntityType)
	filter.EntityKey = stringValue(params.EntityKey)
	filter.Op = stringValue(params.Op)
	if params.Result != nil {
		filter.Result = string(*params.Result)
	}
	if params.SourceSeq != nil {
		if *params.SourceSeq < 0 {
			return filter, false
		}
		filter.SourceSeq = params.SourceSeq
	}
	return filter, true
}

func stringValue(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func toAPIReplicationApplyLog(item repository.ReplicationApplyLog) ReplicationApplyLog {
	return ReplicationApplyLog{
		SourceNode:   item.SourceNode,
		SourceSeq:    item.SourceSeq,
		PeerUrl:      item.PeerURL,
		EntityType:   item.EntityType,
		EntityKey:    item.EntityKey,
		Op:           item.Op,
		Result:       ReplicationApplyLogResult(item.Result),
		Detail:       item.Detail,
		LastError:    optionalString(item.LastError),
		LastErrorAt:  optionalString(item.LastErrorAt),
		FirstSeenAt:  item.FirstSeenAt,
		LastSeenAt:   item.LastSeenAt,
		AttemptCount: item.AttemptCount,
	}
}

func optionalString(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}

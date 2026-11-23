-- 删除与栅栏在同一事务提交。维护须显式 DELETE；事件包不就地更新或替换，恢复另由进程启动栅栏隔离。
CREATE TABLE security_event_read_fence (
    singleton INTEGER PRIMARY KEY CHECK (singleton = 1),
    generation INTEGER NOT NULL CHECK (generation >= 0),
    deleted_from INTEGER,
    deleted_through INTEGER,
    CHECK ((generation = 0 AND deleted_from IS NULL AND deleted_through IS NULL) OR
           (generation > 0 AND deleted_from IS NOT NULL AND deleted_through IS NOT NULL AND deleted_from <= deleted_through))
) STRICT;
INSERT INTO security_event_read_fence (singleton, generation) VALUES (1, 0);

-- 只维护被删事件发生时间的包络；不回填或估算删除数量，也不存事件标识。
CREATE TRIGGER security_event_read_fence_delete AFTER DELETE ON security_event BEGIN
    UPDATE security_event_read_fence SET generation = generation + 1,
        deleted_from = MIN(COALESCE(deleted_from, OLD.occurred_at), OLD.occurred_at),
        deleted_through = MAX(COALESCE(deleted_through, OLD.occurred_at), OLD.occurred_at)
    WHERE singleton = 1;
END;

-- 对象也是列表匹配证据。若维护时父事件已不可寻址，保守地标记全时间域。
CREATE TRIGGER security_event_object_read_fence_delete AFTER DELETE ON security_event_object BEGIN
    UPDATE security_event_read_fence SET generation = generation + 1,
        deleted_from = MIN(COALESCE(deleted_from, 9223372036854775807),
            COALESCE((SELECT occurred_at FROM security_event WHERE sequence = OLD.event_sequence), -9223372036854775808)),
        deleted_through = MAX(COALESCE(deleted_through, -9223372036854775808),
            COALESCE((SELECT occurred_at FROM security_event WHERE sequence = OLD.event_sequence), 9223372036854775807))
    WHERE singleton = 1;
END;

CREATE INDEX security_audit_gap_query_overlap ON security_audit_gap (coverage, window_end, window_start);

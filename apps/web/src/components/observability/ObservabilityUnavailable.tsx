// 生产态观测占位：真实读模型未接入时明确告知，不渲染开发固定样例。
import { Alert } from "@mantine/core";
import { IconInfoCircle } from "@tabler/icons-react";

interface ObservabilityUnavailableProps {
  title: string;
}

export function ObservabilityUnavailable({ title }: ObservabilityUnavailableProps) {
  return (
    <Alert
      variant="light"
      color="gray"
      title="等待真实数据接入"
      icon={<IconInfoCircle size={18} />}
    >
      {title} 的真实观测读模型尚未接入；生产环境不会展示预览指标或预览事件。
    </Alert>
  );
}

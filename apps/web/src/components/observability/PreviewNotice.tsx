import { Alert, Badge, Group, Text } from "@mantine/core";
import { IconInfoCircle } from "@tabler/icons-react";

export function PreviewNotice() {
  return (
    <Alert variant="light" color="blue" icon={<IconInfoCircle size={18} />}>
      <Group gap="xs" wrap="wrap">
        <Badge color="blue" variant="filled">
          预览数据
        </Badge>
        <Text size="sm">仅用于确认页面信息与交互，尚未连接真实观测读模型。</Text>
      </Group>
    </Alert>
  );
}

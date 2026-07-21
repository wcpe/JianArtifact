// 关键交互展台：核验表单校验与全局通知这两类管理端高频交互，
// 让评审在脱离后端时也能走通“提交—反馈”的业务模式。
import { Button, Card, Group, Stack, Text, TextInput } from "@mantine/core";
import { useForm } from "@mantine/form";
import { notifications } from "@mantine/notifications";

export function InteractionsSection() {
  const form = useForm({
    initialValues: { name: "" },
    validate: {
      name: (value) => (value.trim().length === 0 ? "名称不能为空" : null),
    },
  });

  return (
    <Stack gap="lg" data-testid="section-interactions">
      <Card withBorder padding="md">
        <Text fw={600} mb="sm">
          表单校验（Mantine useForm）
        </Text>
        <form
          onSubmit={form.onSubmit((values) => {
            notifications.show({
              title: "创建成功",
              message: `已创建仓库「${values.name}」（验收站示例，未落库）。`,
              color: "green",
            });
            form.reset();
          })}
        >
          <Group align="flex-end">
            <TextInput
              label="仓库名称"
              placeholder="例如 maven-releases"
              {...form.getInputProps("name")}
            />
            <Button type="submit">提交</Button>
          </Group>
        </form>
      </Card>

      <Card withBorder padding="md">
        <Text fw={600} mb="sm">
          全局通知（notifications）
        </Text>
        <Group>
          <Button
            variant="light"
            color="green"
            onClick={() =>
              notifications.show({ title: "操作成功", message: "示例成功通知。", color: "green" })
            }
          >
            成功通知
          </Button>
          <Button
            variant="light"
            color="red"
            onClick={() =>
              notifications.show({ title: "操作失败", message: "示例失败通知。", color: "red" })
            }
          >
            失败通知
          </Button>
        </Group>
      </Card>
    </Stack>
  );
}

// 关键交互展台：核验表单校验、全局通知与危险操作确认弹窗这三类管理端高频交互，
// 让评审在脱离后端时也能走通“提交—反馈”与“确认—执行”的业务模式。
import { Button, Card, Group, Stack, Text, TextInput } from "@mantine/core";
import { useForm } from "@mantine/form";
import { modals } from "@mantine/modals";
import { notifications } from "@mantine/notifications";
import { IconTrash } from "@tabler/icons-react";

export function InteractionsSection() {
  const form = useForm({
    initialValues: { name: "" },
    validate: {
      name: (value) => (value.trim().length === 0 ? "名称不能为空" : null),
    },
  });

  // 与管理端 confirmDanger 一致：居中弹窗、红色确认按钮，确认后弹出成功通知。
  const openDeleteConfirm = () => {
    modals.openConfirmModal({
      title: "删除仓库",
      centered: true,
      children: (
        <Text size="sm">
          确认删除仓库「maven-releases」？该操作不可撤销（验收站示例，未落库）。
        </Text>
      ),
      labels: { confirm: "删除", cancel: "取消" },
      confirmProps: { color: "red" },
      onConfirm: () => {
        notifications.show({
          title: "删除成功",
          message: "已删除仓库（验收站示例）。",
          color: "green",
        });
      },
    });
  };

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

      <Card withBorder padding="md">
        <Text fw={600} mb="sm">
          危险操作确认弹窗（confirm modal）
        </Text>
        <Text c="dimmed" size="sm" mb="sm">
          与管理端删除流程一致：先弹确认弹窗，确认后再执行并反馈。
        </Text>
        <Button color="red" leftSection={<IconTrash size={16} />} onClick={openDeleteConfirm}>
          删除仓库
        </Button>
      </Card>
    </Stack>
  );
}

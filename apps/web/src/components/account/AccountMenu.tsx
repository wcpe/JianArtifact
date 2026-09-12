// 页眉账户菜单：只展示当前会话已有的用户名与角色，并提供唯一的退出动作。
import { Avatar, Badge, Button, Group, Menu, Stack, Text } from "@mantine/core";
import { IconChevronDown, IconLogout } from "@tabler/icons-react";

import type { User } from "../../api/types";

interface AccountMenuProps {
  user: User;
  roleLabel: string;
  accountLabel: string;
  logoutLabel: string;
  loggingOutLabel: string;
  loggingOut: boolean;
  onLogout: () => void;
}

function userInitial(username: string): string {
  return Array.from(username.trim())[0]?.toUpperCase() ?? "?";
}

function AccountIdentity({
  user,
  roleLabel,
  accountLabel,
}: Pick<AccountMenuProps, "user" | "roleLabel" | "accountLabel">) {
  return (
    <Stack data-testid="account-identity" gap="xs" px="sm" py="xs">
      <Text size="xs" c="dimmed" fw={600}>
        {accountLabel}
      </Text>
      <Group gap="xs" wrap="nowrap">
        <Avatar size={32} radius="xl" color="blue">
          {userInitial(user.username)}
        </Avatar>
        <Stack gap={2} miw={0}>
          <Text size="sm" fw={600} truncate>
            {user.username}
          </Text>
          <Badge variant="light" color="blue" size="xs" w="fit-content">
            {roleLabel}
          </Badge>
        </Stack>
      </Group>
    </Stack>
  );
}

export function AccountMenu({
  user,
  roleLabel,
  accountLabel,
  logoutLabel,
  loggingOutLabel,
  loggingOut,
  onLogout,
}: AccountMenuProps) {
  const accessibleName = `${user.username}（${roleLabel}）`;
  return (
    <Menu position="bottom-end" width={224} shadow="md" withinPortal>
      <Menu.Target>
        <Button variant="subtle" size="xs" px="xs" radius="xl" aria-label={accessibleName}>
          <Group gap={6} wrap="nowrap">
            <Avatar data-testid="account-avatar" size={28} radius="xl" color="blue">
              {userInitial(user.username)}
            </Avatar>
            <Stack gap={0} align="flex-start" visibleFrom="sm" miw={0}>
              <Text size="xs" fw={600} truncate maw={112}>
                {user.username}
              </Text>
              <Badge variant="light" color="blue" size="xs">
                {roleLabel}
              </Badge>
            </Stack>
            <IconChevronDown size={14} aria-hidden="true" />
          </Group>
        </Button>
      </Menu.Target>
      <Menu.Dropdown>
        <AccountIdentity user={user} roleLabel={roleLabel} accountLabel={accountLabel} />
        <Menu.Divider />
        <Menu.Item
          color="red"
          leftSection={<IconLogout size={16} />}
          onClick={onLogout}
          disabled={loggingOut}
        >
          {loggingOut ? loggingOutLabel : logoutLabel}
        </Menu.Item>
      </Menu.Dropdown>
    </Menu>
  );
}

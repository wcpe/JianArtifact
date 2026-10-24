// 破坏性操作的确认弹窗：二次确认 + 输入确认词。
//
// 为什么不用 `modals.openConfirmModal`：它的确认按钮不受弹窗内子组件状态控制，
// 于是"输入确认词才能提交"这条防线只能写成**装饰**——用户不输入也能点确认。
// 这里改用受控 `Modal`，把确认按钮的 `disabled` 直接绑到输入值上，让第三层防护
// 成为真正的门，而不是提示文字。
import { Button, Group, Modal, Stack, Text, TextInput } from "@mantine/core";
import { useEffect, useState } from "react";

/** 确认词：中文动作词，与界面语言一致，避免用英文 "DELETE" 造成割裂。 */
export const DESTRUCTIVE_CONFIRM_WORD = "清理";

export interface DestructiveConfirmModalProps {
  opened: boolean;
  title: string;
  /** 说明将发生什么（含条目数与预计释放）。 */
  message: string;
  /** 是否需要输入确认词才可提交（不可逆操作用 true）。 */
  requirePhrase?: boolean;
  confirmLabel?: string;
  cancelLabel?: string;
  onConfirm: () => void;
  onClose: () => void;
}

export function DestructiveConfirmModal({
  opened,
  title,
  message,
  requirePhrase = true,
  confirmLabel = DESTRUCTIVE_CONFIRM_WORD,
  cancelLabel = "取消",
  onConfirm,
  onClose,
}: DestructiveConfirmModalProps) {
  const [typed, setTyped] = useState("");

  // 每次打开都重置：否则上次残留的输入会让下一次确认直接通过。
  useEffect(() => {
    if (opened) setTyped("");
  }, [opened]);

  const phraseOk = !requirePhrase || typed.trim() === DESTRUCTIVE_CONFIRM_WORD;

  return (
    <Modal opened={opened} onClose={onClose} title={title} centered>
      <Stack gap="sm">
        <Text size="sm" style={{ whiteSpace: "pre-wrap" }}>
          {message}
        </Text>
        {requirePhrase ? (
          <>
            <Text size="sm" fw={500}>
              请输入「{DESTRUCTIVE_CONFIRM_WORD}」以确认
            </Text>
            <TextInput
              aria-label="确认词"
              placeholder={DESTRUCTIVE_CONFIRM_WORD}
              value={typed}
              onChange={(e) => setTyped(e.currentTarget.value)}
              data-confirm-phrase-ok={phraseOk ? "true" : "false"}
            />
          </>
        ) : null}
        <Group justify="flex-end" gap="xs">
          <Button variant="default" onClick={onClose}>
            {cancelLabel}
          </Button>
          <Button color="red" disabled={!phraseOk} onClick={onConfirm} data-confirm-submit>
            {confirmLabel}
          </Button>
        </Group>
      </Stack>
    </Modal>
  );
}

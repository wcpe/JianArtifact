// 破坏性操作的确认弹窗：二次确认 + 输入确认词。
//
// 为什么不用 `modals.openConfirmModal`：它的确认按钮不受弹窗内子组件状态控制，
// 于是"输入确认词才能提交"这条防线只能写成**装饰**——用户不输入也能点确认。
// 这里改用受控 `Modal`，把确认按钮的 `disabled` 直接绑到输入值上，让第三层防护
// 成为真正的门，而不是提示文字。
import { Button, Group, Modal, Stack, Text, TextInput } from "@mantine/core";
import { useEffect, useState } from "react";
import { useTranslation } from "react-i18next";

/**
 * 确认词的 i18n 键：**按当前界面语言求值**，不能是模块级常量。
 *
 * 早先它是 `const DESTRUCTIVE_CONFIRM_WORD = "清理"`，切到英文后提示语与占位符
 * 仍是中文，用户得对着英文界面输入中文才能确认——而这道防线的作用是"让人有意识地
 * 停一下"，不是考语言。中文侧取值仍是「清理」，既有测试的语义不变。
 *
 * 导出的是**键名而非文案**：任何在渲染期取文案的地方都必须走 `useTranslation`，
 * 把文案固化到常量会让它重新脱离语言切换。
 */
export const CONFIRM_PHRASE_KEY = "maintenance.confirmPhrase";

export interface DestructiveConfirmModalProps {
  opened: boolean;
  title: string;
  /** 说明将发生什么（含条目数与预计释放）。 */
  message: string;
  /** 是否需要输入确认词才可提交（不可逆操作用 true）。 */
  requirePhrase?: boolean;
  /**
   * 确认按钮文案。省略时**复用确认词本身**（"清理" / "Purge"），
   * 与既有行为一致；调用方不要再传固定字符串，否则该按钮会脱离语言切换。
   */
  confirmLabel?: string;
  cancelLabel?: string;
  /**
   * 提交进行中：由调用方在请求在途时置 true。
   *
   * 没有它时确认按钮只受确认词控制，请求发出后到弹窗关闭之间按钮仍可点，
   * 连点会重复发出同一批不可逆请求（第二次往往撞 409 或重复删除），
   * 界面看起来像"点了没反应"或"报错了"。禁用 + loading 让点击有且只有一次。
   */
  pending?: boolean;
  onConfirm: () => void;
  onClose: () => void;
}

export function DestructiveConfirmModal({
  opened,
  title,
  message,
  requirePhrase = true,
  confirmLabel,
  cancelLabel,
  pending = false,
  onConfirm,
  onClose,
}: DestructiveConfirmModalProps) {
  const { t } = useTranslation();
  const [typed, setTyped] = useState("");
  const phrase = t(CONFIRM_PHRASE_KEY);

  // 每次打开都重置：否则上次残留的输入会让下一次确认直接通过。
  useEffect(() => {
    if (opened) setTyped("");
  }, [opened]);

  // 大小写与首尾空白都归一：英文确认词是单词，用户打成 "delete" / "Delete " 都该放行
  // ——这道门要挡的是"没意识到自己在做什么"，不是拼写。中文侧无大小写，行为不变。
  const phraseOk = !requirePhrase || typed.trim().toLowerCase() === phrase.trim().toLowerCase();

  return (
    <Modal opened={opened} onClose={onClose} title={title} centered>
      <Stack gap="sm">
        <Text size="sm" style={{ whiteSpace: "pre-wrap" }}>
          {message}
        </Text>
        {requirePhrase ? (
          <>
            <Text size="sm" fw={500}>
              {t("maintenance.confirmPhrasePrompt", { phrase })}
            </Text>
            <TextInput
              // 打开即可输入：确认词是这道防线的唯一入口，还要先点一下输入框才能打字
              // 只是无谓的一次点击。
              //
              // 必须同时给 `data-autofocus`：Mantine 的 FocusTrap 会在挂载后把焦点移到
              // 容器内第一个可聚焦元素（这里是右上角关闭按钮），它会盖掉 React 的
              // `autoFocus`；`data-autofocus` 才是它认的初始焦点标记。
              autoFocus
              data-autofocus
              aria-label={t("maintenance.confirmPhraseLabel")}
              placeholder={phrase}
              value={typed}
              onChange={(e) => setTyped(e.currentTarget.value)}
              data-confirm-phrase-ok={phraseOk ? "true" : "false"}
            />
          </>
        ) : null}
        <Group justify="flex-end" gap="xs">
          <Button variant="default" onClick={onClose} disabled={pending}>
            {cancelLabel ?? t("common.cancel")}
          </Button>
          <Button
            color="red"
            disabled={!phraseOk || pending}
            loading={pending}
            onClick={onConfirm}
            data-confirm-submit
          >
            {confirmLabel ?? phrase}
          </Button>
        </Group>
      </Stack>
    </Modal>
  );
}

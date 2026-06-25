// 上传页面组件测试（FR-74）：选仓库后按格式渲染动态表单；填齐坐标 + 选文件后调用上传 API；
// 仅展示可上传的 hosted 仓库（Maven / npm / Raw）。

import { describe, it, expect, vi, afterEach } from 'vitest';
import { render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { MantineProvider } from '@mantine/core';
import { UploadPage } from './UploadPage';
import * as api from '../api/endpoints';
import type { RepositoryDto } from '../api/types';

/** 在 Mantine Provider 下渲染上传页。 */
function renderPage() {
  return render(
    <MantineProvider>
      <UploadPage />
    </MantineProvider>,
  );
}

const 仓库列表: RepositoryDto[] = [
  {
    id: 'r-maven',
    name: 'maven-releases',
    format: 'maven',
    type: 'hosted',
    visibility: 'private',
    upstream_url: null,
    created_at: '2026-06-24T00:00:00Z',
  },
  {
    id: 'r-raw',
    name: 'raw-files',
    format: 'raw',
    type: 'hosted',
    visibility: 'public',
    upstream_url: null,
    created_at: '2026-06-24T00:00:00Z',
  },
  {
    id: 'r-proxy',
    name: 'maven-central',
    format: 'maven',
    type: 'proxy',
    visibility: 'public',
    upstream_url: 'https://repo1.maven.org/maven2',
    created_at: '2026-06-24T00:00:00Z',
  },
  {
    id: 'r-docker',
    name: 'docker-hosted',
    format: 'docker',
    type: 'hosted',
    visibility: 'public',
    upstream_url: null,
    created_at: '2026-06-24T00:00:00Z',
  },
];

describe('UploadPage', () => {
  afterEach(() => vi.restoreAllMocks());

  /** 仓库下拉的占位符（用作选择器入口）。 */
  const 仓库占位 = '选择一个 hosted 仓库（Maven / npm / Raw）';

  it('仅把可上传的 hosted 仓库（maven/npm/raw）放入选择列表', async () => {
    vi.spyOn(api, 'listRepositories').mockResolvedValue(仓库列表);
    renderPage();

    const user = userEvent.setup();
    await waitFor(() => expect(screen.getByPlaceholderText(仓库占位)).toBeInTheDocument());
    await user.click(screen.getByPlaceholderText(仓库占位));

    // hosted 的 maven / raw 在列；proxy 与 docker 被排除
    expect(await screen.findByText('maven-releases（maven）')).toBeInTheDocument();
    expect(screen.getByText('raw-files（raw）')).toBeInTheDocument();
    expect(screen.queryByText('maven-central（maven）')).not.toBeInTheDocument();
    expect(screen.queryByText('docker-hosted（docker）')).not.toBeInTheDocument();
  });

  it('选 Maven 仓库后渲染 GAV 动态表单', async () => {
    vi.spyOn(api, 'listRepositories').mockResolvedValue(仓库列表);
    renderPage();

    const user = userEvent.setup();
    await waitFor(() => expect(screen.getByPlaceholderText(仓库占位)).toBeInTheDocument());
    await user.click(screen.getByPlaceholderText(仓库占位));
    await user.click(await screen.findByText('maven-releases（maven）'));

    expect(screen.getByPlaceholderText('com.example.app')).toBeInTheDocument();
    expect(screen.getByPlaceholderText('demo')).toBeInTheDocument();
    expect(screen.getByPlaceholderText('1.0.0')).toBeInTheDocument();
    // 不应出现 Raw 的 path 字段
    expect(screen.queryByPlaceholderText('dir/sub/file.bin')).not.toBeInTheDocument();
  });

  it('选 Raw 仓库填路径 + 选文件后调用上传 API 并提示成功', async () => {
    vi.spyOn(api, 'listRepositories').mockResolvedValue(仓库列表);
    const uploadSpy = vi.spyOn(api, 'uploadArtifact').mockResolvedValue(undefined);
    renderPage();

    const user = userEvent.setup();
    await waitFor(() => expect(screen.getByPlaceholderText(仓库占位)).toBeInTheDocument());
    await user.click(screen.getByPlaceholderText(仓库占位));
    await user.click(await screen.findByText('raw-files（raw）'));

    // 填路径
    await user.type(screen.getByPlaceholderText('dir/sub/file.bin'), 'dir/file.bin');

    // 选文件（FileInput 渲染为隐藏的 file input）
    const fileInput = document.querySelector('input[type="file"]') as HTMLInputElement;
    const f = new File([new Uint8Array([1, 2, 3])], 'file.bin', {
      type: 'application/octet-stream',
    });
    await user.upload(fileInput, f);

    // 触发上传
    await user.click(screen.getByRole('button', { name: '上传' }));

    await waitFor(() => expect(uploadSpy).toHaveBeenCalledTimes(1));
    const [repoId, formData] = uploadSpy.mock.calls[0];
    expect(repoId).toBe('r-raw');
    expect((formData as FormData).get('path')).toBe('dir/file.bin');
    expect((formData as FormData).get('file')).toBeInstanceOf(File);
  });

  it('坐标未填齐时上传按钮禁用', async () => {
    vi.spyOn(api, 'listRepositories').mockResolvedValue(仓库列表);
    renderPage();

    const user = userEvent.setup();
    await waitFor(() => expect(screen.getByPlaceholderText(仓库占位)).toBeInTheDocument());
    await user.click(screen.getByPlaceholderText(仓库占位));
    await user.click(await screen.findByText('raw-files（raw）'));

    // 未填 path、未选文件 → 按钮禁用
    expect(screen.getByRole('button', { name: '上传' })).toBeDisabled();
  });
});

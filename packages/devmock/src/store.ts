// 内存态数据存储：为 MSW 处理器提供可增删改查的管理面数据。
// 仅用于开发态（浏览器 worker）与测试（Node server）Mock，不进入生产构建。
// 类型绑定 schema.gen.ts（与 api/openapi.yaml 同源），保证 mock 数据不偏离契约。
import type { components } from "./schema.gen";
import { mockVolumeFactor, subscribeMockConsole } from "./console";
import { resetObservabilityStore } from "./observability";
import { MOCK_APP_VERSION, MOCK_MIGRATION_VERSION } from "./version";

type Schemas = components["schemas"];

export type User = Schemas["User"];
export type Token = Schemas["Token"];
export type TokenCreated = Schemas["TokenCreated"];
export type Repository = Schemas["Repository"];
export type ConnectionStatus = Schemas["ConnectionStatus"];
export type AclEntry = Schemas["AclEntry"];
export type StatusInfo = Schemas["StatusInfo"];
export type AssetSummary = Schemas["AssetSummary"];
export type AssetList = Schemas["AssetList"];
export type UsageInfo = Schemas["UsageInfo"];
export type UsageSnippet = Schemas["UsageSnippet"];
export type MigrationTask = Schemas["MigrationTask"];
export type MigrationPlan = Schemas["MigrationPlan"];
export type MigrationReport = Schemas["MigrationReport"];
export type MigrationSourceConfig = Schemas["MigrationSourceConfig"];
export type MigrationSourceAuth = Schemas["MigrationSourceAuth"];
export type MigrationSourceAuthType = Schemas["MigrationSourceAuthType"];
export type RemoteNexusSourceConfig = Schemas["RemoteNexusSourceConfig"];
// FR-132：节点备份与搬迁
export type BackupPackage = Schemas["BackupPackage"];
export type BackupPackageList = Schemas["BackupPackageList"];
export type BackupLink = Schemas["BackupLink"];
// FR-134 / FR-137：写入冻结窗口与从 URL 导入备份包
export type WriteFreezeState = Schemas["WriteFreezeState"];
export type FreezeWritesRequest = Schemas["FreezeWritesRequest"];
export type BackupImport = Schemas["BackupImport"];
export type BackupImportList = Schemas["BackupImportList"];
export type BackupImportStatus = Schemas["BackupImportStatus"];
export type BackupImportOrigin = Schemas["BackupImportOrigin"];
export type CreateBackupImportRequest = Schemas["CreateBackupImportRequest"];

export interface PublishPolicy {
  userId: number;
  username: string;
  webLoginDisabled: boolean;
  repository: string;
  allowedPrefixes: string[];
  maxAssetsHour: number;
  maxBytesDay: number;
  maxFileBytes: number;
  immutableRelease: boolean;
}

/** 管理端可保存的服务设置；不包含部署和集群机密。 */
export interface ServiceSettings {
  anonymousAccess: boolean;
  publicUrl: string;
  upstreamTimeout: number;
  syncInterval: number;
  /** 允许访问的域名白名单（空数组表示不限制）；与真实后端一致按列表存储。 */
  allowedHosts: string[];
  /** 回源 Token 校验（FR-130）；节点本地配置。 */
  originTokenEnabled: boolean;
  originTokenHeader: string;
  originTokenValue: string;
}

/** 开发/测试态签发的固定会话令牌明文；鉴权守卫据此放行。 */
export const MOCK_TOKEN = "mock.jwt.token";
/** 第二个管理员会话，仅供审计确认并发 Mock 验收使用。 */
export const MOCK_SECOND_ADMIN_TOKEN = "mock.jwt.token:admin-2";

interface StoredToken extends Token {
  /** 明文令牌仅签发时返回一次，此处留存以模拟“再次列表不含明文”。 */
  plaintext: string;
  /** 令牌属于创建它的用户，列表和吊销均不得跨主体访问。 */
  ownerId: number;
}

interface State {
  initialized: boolean;
  version: string;
  migrationVersion: string;
  users: User[];
  tokens: StoredToken[];
  repositories: Repository[];
  /**
   * 数据量档位克隆出的仓库名（`<seed>-v<r>`）。它们是**真实存在于 store** 的仓库
   * （不是只在响应里改名的假条目），档位切换时按这份账本回收重放。
   */
  volumeClones: string[];
  /** 上一次物化的档位倍数；1 = 仅种子。用于避免同档重复物化（也会复活被删掉的克隆）。 */
  volumeFactor: number;
  /** FR-114：proxy 仓库上游连接状态（内存态，按仓库名；hosted/group 不维护）。 */
  connStatus: Record<string, ConnectionStatus>;
  acls: Record<string, AclEntry[]>;
  assets: Record<string, AssetSummary[]>;
  migrations: MigrationTask[];
  publishPolicies: Record<string, Omit<PublishPolicy, "userId" | "username" | "repository">>;
  seq: { user: number; token: number; repo: number; migration: number; operation: number };
  /** FR-66：实例级匿名访问开关（默认开）。 */
  anonymousAccessEnabled: boolean;
  /** 管理端服务设置；集群拓扑仍由部署配置决定。 */
  serviceSettings: Omit<ServiceSettings, "anonymousAccess">;
  /** FR-86：复制同步调度启停（默认开）。 */
}

function seed(): State {
  return {
    initialized: true,
    version: MOCK_APP_VERSION,
    migrationVersion: MOCK_MIGRATION_VERSION,
    users: [
      {
        id: 1,
        username: "admin",
        role: "admin",
        status: "active",
        webLoginDisabled: false,
        createdAt: "2026-01-01T00:00:00Z",
      },
      {
        id: 2,
        username: "developer",
        role: "user",
        status: "active",
        webLoginDisabled: false,
        createdAt: "2026-01-02T00:00:00Z",
      },
      {
        id: 3,
        username: "admin-2",
        role: "admin",
        status: "active",
        webLoginDisabled: false,
        createdAt: "2026-01-03T00:00:00Z",
      },
      // 状态多样性：停用账号 + 仅协议发布（禁 Web 登录）账号，便于核对列表的停用/受限呈现。
      {
        id: 4,
        username: "ci-runner",
        role: "user",
        status: "disabled",
        webLoginDisabled: true,
        createdAt: "2026-01-05T00:00:00Z",
      },
      {
        id: 5,
        username: "auditor",
        role: "user",
        status: "active",
        webLoginDisabled: true,
        createdAt: "2026-01-07T00:00:00Z",
      },
    ],
    tokens: [
      {
        id: 1,
        name: "ci",
        createdAt: "2026-01-03T00:00:00Z",
        plaintext: "jat_seedci",
        ownerId: 1,
      },
      {
        id: 2,
        name: "publisher-bot",
        createdAt: "2026-01-08T09:12:00Z",
        plaintext: "jat_seedpublisherbot",
        ownerId: 1,
      },
      {
        id: 3,
        name: "nightly-publisher",
        createdAt: "2026-01-15T02:30:00Z",
        plaintext: "jat_seednightly",
        ownerId: 1,
      },
      {
        id: 4,
        name: "backup-cron",
        createdAt: "2026-01-22T03:00:00Z",
        plaintext: "jat_seedbackupcron",
        ownerId: 1,
      },
    ],
    repositories: [
      {
        id: 1,
        name: "maven-releases",
        format: "maven",
        type: "hosted",
        visibility: "private",
        description: "团队 Maven release 制品库",
        online: true,
        createdAt: "2026-01-01T00:00:00Z",
        artifactCount: 1284,
        totalSize: 8589934592,
      },
      {
        id: 2,
        name: "npm-proxy",
        format: "npm",
        type: "proxy",
        visibility: "public",
        remoteUrl: "https://registry.npmjs.org",
        online: true,
        createdAt: "2026-01-02T00:00:00Z",
        artifactCount: 5240,
        totalSize: 12884901888,
      },
      {
        id: 3,
        name: "raw-hosted",
        format: "raw",
        type: "hosted",
        visibility: "private",
        online: true,
        createdAt: "2026-01-03T00:00:00Z",
        artifactCount: 82,
        totalSize: 1503238144,
      },
      // v0.8.0：补充仓库状态面板样例——与 dashboard 告警对应的被阻止 proxy + 状态多样性。
      // 种子总数控制在 9（PAGE_SIZE=10 内），保证新建仓库后仍出现在列表第 1 页。
      {
        id: 4,
        name: "maven-papermc",
        format: "maven",
        type: "proxy",
        visibility: "public",
        remoteUrl: "https://papermc.io/repo/v2/releases",
        online: true,
        createdAt: "2026-01-04T00:00:00Z",
        artifactCount: 980,
        totalSize: 4294967296,
      },
      {
        id: 5,
        name: "maven-central",
        format: "maven",
        type: "proxy",
        visibility: "public",
        remoteUrl: "https://repo1.maven.org/maven2",
        online: true,
        createdAt: "2026-01-04T00:00:00Z",
        artifactCount: 12480,
        totalSize: 38654706022,
      },
      {
        id: 6,
        name: "maven-airgame",
        format: "maven",
        type: "proxy",
        visibility: "public",
        remoteUrl: "https://dl.airgame.io/repository/public",
        online: true,
        createdAt: "2026-01-05T00:00:00Z",
        artifactCount: 620,
        totalSize: 2147483648,
      },
      {
        id: 7,
        name: "docker-hub",
        format: "docker",
        type: "proxy",
        visibility: "public",
        remoteUrl: "https://registry-1.docker.io",
        online: true,
        createdAt: "2026-01-05T00:00:00Z",
        artifactCount: 3860,
        totalSize: 15032385536,
      },
      {
        id: 8,
        name: "pypi-mirror",
        format: "pypi",
        type: "proxy",
        visibility: "public",
        remoteUrl: "https://pypi.org/simple",
        online: true,
        createdAt: "2026-01-06T00:00:00Z",
        artifactCount: 2140,
        totalSize: 6442450944,
      },
      {
        id: 9,
        name: "gomod-proxy",
        format: "gomod",
        type: "proxy",
        visibility: "public",
        remoteUrl: "https://proxy.golang.org",
        online: true,
        createdAt: "2026-01-06T00:00:00Z",
        artifactCount: 1730,
        totalSize: 3221225472,
      },
    ],
    // 只保留 maven-releases 一条：ACL 直接决定"普通用户可读仓库"与"私有仓库 403"的权限语义，
    // 契约测试按此断言（可读仓库数、raw-hosted 拒绝读），不宜为了列表好看而增补。
    volumeClones: [],
    volumeFactor: 1,
    acls: { "maven-releases": [{ subjectId: 2, action: "read" }] },
    connStatus: {
      // FR-114：各 proxy 上游连接状态内存态——2 可用 / 4 自动阻止 / 1 不可用，供列表徽章与状态面板演示。
      "npm-proxy": { status: "AVAILABLE", description: "上游可用" },
      "gomod-proxy": { status: "AVAILABLE", description: "上游可用" },
      "maven-papermc": {
        status: "AUTO_BLOCKED",
        description: "上游连续不可达，已进入自动阻止窗口",
      },
      "maven-central": {
        status: "AUTO_BLOCKED",
        description: "上游连续不可达，已进入自动阻止窗口",
      },
      "docker-hub": { status: "AUTO_BLOCKED", description: "上游连续不可达，已进入自动阻止窗口" },
      "pypi-mirror": { status: "AUTO_BLOCKED", description: "上游连续不可达，已进入自动阻止窗口" },
      "maven-airgame": { status: "UNAVAILABLE", description: "上游暂时不可用" },
    },
    assets: {
      "maven-releases": [
        {
          path: "com/example/app/1.0.0/app-1.0.0.jar",
          size: 20480,
          hash: "1cbc8dc671bf6bff02cdb5132104dd0d08bdf2b8b89a95b68d9aaf6fe1fe1761",
          contentType: "application/java-archive",
          updatedAt: "2026-01-04T00:00:00Z",
        },
        {
          path: "com/example/app/1.0.0/app-1.0.0.pom",
          size: 512,
          hash: "ed680a8fa52ee95ef74cf9daf6d7cdaca7b4a09753de686ddfc435ae9c715c9b",
          contentType: "application/xml",
          updatedAt: "2026-01-04T00:00:00Z",
        },
        {
          path: "com/example/app/1.1.0/app-1.1.0.jar",
          size: 22528,
          hash: "e8712134d8087f4518ac45dda8d3277e4fcb70b5364fa3bb9295f91efbfd32eb",
          contentType: "application/java-archive",
          updatedAt: "2026-01-09T11:20:00Z",
        },
        {
          path: "com/example/app/1.1.0/app-1.1.0.pom",
          size: 528,
          hash: "57473262901c16971ac9c5e80746df56d83173ca2b76bc33060da74c6e6e76eb",
          contentType: "application/xml",
          updatedAt: "2026-01-09T11:20:00Z",
        },
        {
          path: "com/example/checkout/2.4.1/checkout-2.4.1.jar",
          size: 4124672,
          hash: "053b808e770b5b6fb364dd9a14fe8a48fe6c99d2738ce3ae29d7b2116c485820",
          contentType: "application/java-archive",
          updatedAt: "2026-01-12T08:41:00Z",
        },
        {
          path: "com/example/checkout/2.4.1/checkout-2.4.1.pom",
          size: 6842,
          hash: "6ebea2e581d698304487205fe1dc1565008310cd749996732d52a9f0082ff6f6",
          contentType: "application/xml",
          updatedAt: "2026-01-12T08:41:00Z",
        },
        {
          path: "com/example/payment/1.9.0/payment-1.9.0.jar",
          size: 3586112,
          hash: "b3f744c3ac81b2f81ee71181dc083742f37a836fe7281cccaf2ba32351dd3efc",
          contentType: "application/java-archive",
          updatedAt: "2026-01-18T14:05:00Z",
        },
        {
          path: "com/example/payment/1.9.0/payment-1.9.0.pom",
          size: 5917,
          hash: "13f4b91ecd5adf2074bcf8dd791aaedc1e1d73c3514b75cb2b6b74d8ff340eff",
          contentType: "application/xml",
          updatedAt: "2026-01-18T14:05:00Z",
        },
        {
          path: "com/example/maven-metadata.xml",
          size: 2184,
          hash: "dd5d5aa25ddcd7a81d6287d4e4387913baffde2e2d27d10ec24425d94e6eb8f9",
          contentType: "application/xml",
          updatedAt: "2026-01-18T14:06:00Z",
        },
        {
          path: "org/example/shared/0.9.3/shared-0.9.3.jar",
          size: 786432,
          hash: "98d9d7b7103993b4829df0865486374004a354ae0d8357d6fbd14b601f6a7988",
          contentType: "application/java-archive",
          updatedAt: "2026-01-20T10:15:00Z",
        },
        {
          path: "org/example/shared/0.9.3/shared-0.9.3.pom",
          size: 4912,
          hash: "837a064c1143d43a6afb3d590d7ebf3f73729ecaa1e590261f5d53104f06855a",
          contentType: "application/xml",
          updatedAt: "2026-01-20T10:15:00Z",
        },
      ],
      "maven-central": [
        {
          path: "org/springframework/spring-core/6.1.4/spring-core-6.1.4.jar",
          size: 1884160,
          hash: "36329256388fe22d55d4676e3480b05dd48c7c5920e16b3d573f4cb78ba0acb0",
          contentType: "application/java-archive",
          updatedAt: "2026-01-07T03:12:00Z",
        },
        {
          path: "org/springframework/spring-core/6.1.4/spring-core-6.1.4.pom",
          size: 8142,
          hash: "774a85251d32937c13b927f7a1647f000b043d8efc424f88346dffa78e8ee151",
          contentType: "application/xml",
          updatedAt: "2026-01-07T03:12:00Z",
        },
        {
          path: "org/springframework/boot/spring-boot/3.2.3/spring-boot-3.2.3.jar",
          size: 1622016,
          hash: "c6f4606b9b447278bb1009f6f34b36903c5bc65249000be978c35f37aa911c3c",
          contentType: "application/java-archive",
          updatedAt: "2026-01-08T05:44:00Z",
        },
        {
          path: "org/apache/commons/commons-lang3/3.14.0/commons-lang3-3.14.0.jar",
          size: 638976,
          hash: "f199cdea6934f44fb2533b2b42ad6505aa2df6b0cc3f972758e79298adeb9bdf",
          contentType: "application/java-archive",
          updatedAt: "2026-01-08T05:44:00Z",
        },
        {
          path: "com/google/guava/guava/33.0.0-jre/guava-33.0.0-jre.jar",
          size: 3047424,
          hash: "83db20da90aade643b38a43615f1213dc7df070131f714739f2f3461e8fac967",
          contentType: "application/java-archive",
          updatedAt: "2026-01-11T19:02:00Z",
        },
        {
          path: "org/junit/jupiter/junit-jupiter/5.10.2/junit-jupiter-5.10.2.jar",
          size: 152576,
          hash: "05762f34064779092b8a297e4fff898fdbd9a3ec96b598edc6e56d17cec174b6",
          contentType: "application/java-archive",
          updatedAt: "2026-01-14T07:31:00Z",
        },
      ],
      "npm-proxy": [
        {
          path: "lodash/-/lodash-4.17.21.tgz",
          size: 1048576,
          hash: "79ea61c0b99294ed81dfe3f3fda27ac3b06a9114d8331ac1e250fcf4a343fdcb",
          contentType: "application/gzip",
          updatedAt: "2026-01-06T12:00:00Z",
        },
        {
          path: "react/-/react-18.3.1.tgz",
          size: 312320,
          hash: "ab7f0f00c84658fc653aa912529ee01e810f6cb16ae5420e50a6ccff2884c80d",
          contentType: "application/gzip",
          updatedAt: "2026-01-06T12:01:00Z",
        },
        {
          path: "react-dom/-/react-dom-18.3.1.tgz",
          size: 1286144,
          hash: "63cc1510605b36094239fa9c8f99350ab68ab264efd4103c34e8e169977084a6",
          contentType: "application/gzip",
          updatedAt: "2026-01-06T12:01:00Z",
        },
        {
          path: "typescript/-/typescript-5.6.3.tgz",
          size: 12582912,
          hash: "3f636e4073df06cc84dc0b22c868e63624acb934c357ee7c96c2dd87e216d554",
          contentType: "application/gzip",
          updatedAt: "2026-01-10T09:22:00Z",
        },
        {
          path: "vite/-/vite-5.4.10.tgz",
          size: 2621440,
          hash: "d42b6979e3693680821e602ccdf560aa5d879d6990f285987b1649949ed56ed4",
          contentType: "application/gzip",
          updatedAt: "2026-01-10T09:23:00Z",
        },
        {
          path: "axios/-/axios-1.7.7.tgz",
          size: 528384,
          hash: "352116a932af5f30fa3ee67b4c07fa51c7d0043c920b8357a9c1a5760580e7a2",
          contentType: "application/gzip",
          updatedAt: "2026-01-13T16:48:00Z",
        },
      ],
      "docker-hub": [
        {
          path: "library/nginx/manifests/1.27-alpine",
          size: 4096,
          hash: "a1c1f751df6a47222e503ba6261a5c8b76a8c0f5e1aa7a50915d77a8f92717d6",
          contentType: "application/vnd.oci.image.manifest.v1+json",
          updatedAt: "2026-01-07T21:10:00Z",
        },
        {
          path: "library/nginx/blobs/sha256/9f1b7c1e4d2a8b3c5e6f7a8b9c0d1e2f3a4b5c6d7e8f9a0b1c2d3e4f5a6b7c8d",
          size: 52428800,
          hash: "06ade8bf37bb1ed853d9dd2461d36afeed3c250c1f7538caf1a80d6193819320",
          contentType: "application/octet-stream",
          updatedAt: "2026-01-07T21:10:00Z",
        },
        {
          path: "library/redis/manifests/7.4-alpine",
          size: 3842,
          hash: "426d54a3941540d9da9659b3d83e1735147798a3e9d0188e0bda80d0a23b12bf",
          contentType: "application/vnd.oci.image.manifest.v1+json",
          updatedAt: "2026-01-09T04:35:00Z",
        },
        {
          path: "library/redis/blobs/sha256/1a2b3c4d5e6f7a8b9c0d1e2f3a4b5c6d7e8f9a0b1c2d3e4f5a6b7c8d9e0f1a2b",
          size: 41943040,
          hash: "6a1ed0b8e00e19d0d1b0a71762fa6e39d0ebad3be846d77f8aa7fb69b66e078b",
          contentType: "application/octet-stream",
          updatedAt: "2026-01-09T04:35:00Z",
        },
        {
          path: "library/postgres/manifests/16-alpine",
          size: 4224,
          hash: "7eb084c7fdd46f2e2e26d2a917929f3fad2d68883e2246f89cb6f0c10c458c1f",
          contentType: "application/vnd.oci.image.manifest.v1+json",
          updatedAt: "2026-01-15T18:27:00Z",
        },
        {
          path: "library/postgres/blobs/sha256/2b3c4d5e6f7a8b9c0d1e2f3a4b5c6d7e8f9a0b1c2d3e4f5a6b7c8d9e0f1a2b3c",
          size: 89128960,
          hash: "ad00aa9b75f500a16aa0b800e718111bb66bd250c112478539e0be94c3c5d9b6",
          contentType: "application/octet-stream",
          updatedAt: "2026-01-15T18:27:00Z",
        },
      ],
      "pypi-mirror": [
        {
          path: "packages/requests/2.32.3/requests-2.32.3-py3-none-any.whl",
          size: 131072,
          hash: "e62d9e2447f350a1f60297d03a75f743a315a13a1479307bbe3a46c105693467",
          contentType: "application/octet-stream",
          updatedAt: "2026-01-06T22:40:00Z",
        },
        {
          path: "packages/requests/2.32.3/requests-2.32.3.tar.gz",
          size: 112640,
          hash: "99a9e40585e54243e6edcb74cfcd660a7196758710a85e1dbb567a61134aa826",
          contentType: "application/gzip",
          updatedAt: "2026-01-06T22:40:00Z",
        },
        {
          path: "packages/flask/3.0.3/flask-3.0.3-py3-none-any.whl",
          size: 98304,
          hash: "54ebd9adbe71deb0ede99bc9cff45c8c62a77088b678ae80de67777654f79466",
          contentType: "application/octet-stream",
          updatedAt: "2026-01-12T11:05:00Z",
        },
        {
          path: "packages/numpy/2.1.2/numpy-2.1.2-cp312-cp312-manylinux.whl",
          size: 18874368,
          hash: "a5be53614ecff89bf9fde896229cc5774437c7af359d5fdbd17f89c9afe0e73a",
          contentType: "application/octet-stream",
          updatedAt: "2026-01-19T13:55:00Z",
        },
      ],
      "gomod-proxy": [
        {
          path: "github.com/gin-gonic/gin/@v/v1.10.0.zip",
          size: 786432,
          hash: "72316caa9195882196afb4bee4b535b9a2cc62314f6a3e1a2a5c00a265e82662",
          contentType: "application/zip",
          updatedAt: "2026-01-05T15:12:00Z",
        },
        {
          path: "github.com/gin-gonic/gin/@v/v1.10.0.mod",
          size: 4096,
          hash: "a9132f5ed8ba8e8d90320e83d146516bb9856b2f75505c5c5b828656349df67a",
          contentType: "text/plain",
          updatedAt: "2026-01-05T15:12:00Z",
        },
        {
          path: "github.com/stretchr/testify/@v/v1.9.0.zip",
          size: 524288,
          hash: "8b20ca37de061ee880868326b90497157045a91f41f3011aece05e4369e6a3f2",
          contentType: "application/zip",
          updatedAt: "2026-01-11T06:38:00Z",
        },
        {
          path: "golang.org/x/sync/@v/v0.8.0.zip",
          size: 262144,
          hash: "86967709469802a3def05d4a4b37fc080ffdc19901667cda7677ac51b943078e",
          contentType: "application/zip",
          updatedAt: "2026-01-17T20:14:00Z",
        },
      ],
      "maven-papermc": [
        {
          path: "com/destroystokyo/paper/paper-api/1.20.1-R0.1-SNAPSHOT/paper-api-1.20.1-R0.1-SNAPSHOT.jar",
          size: 2097152,
          hash: "c43e094a5d15f55a1c1414f87f1f2f204c9a3048104763ba3ba9ab01ee61a6b7",
          contentType: "application/java-archive",
          updatedAt: "2026-01-08T17:20:00Z",
        },
        {
          path: "com/destroystokyo/paper/paper-api/1.20.1-R0.1-SNAPSHOT/paper-api-1.20.1-R0.1-SNAPSHOT.pom",
          size: 6144,
          hash: "831c4357ab2782f8d558593ef049e21c33e0a5dc75416a6e69dd61539e8ee67d",
          contentType: "application/xml",
          updatedAt: "2026-01-08T17:20:00Z",
        },
        {
          path: "io/papermc/paper/paper-server/1.20.1-R0.1-SNAPSHOT/paper-server-1.20.1-R0.1-SNAPSHOT.jar",
          size: 52428800,
          hash: "1a9f67c491fbc323848b5a7aa312ea7baf759aa7f976d30b7935cc43e91c2ea1",
          contentType: "application/java-archive",
          updatedAt: "2026-01-08T17:21:00Z",
        },
      ],
      "maven-airgame": [
        {
          path: "com/airgame/api/airgame-api/2.1.0/airgame-api-2.1.0.jar",
          size: 1572864,
          hash: "76bdcbe2f4e5bc3ef3e5fccd65797db4bfc9d1bb93573668b282f44ad664040c",
          contentType: "application/java-archive",
          updatedAt: "2026-01-10T14:33:00Z",
        },
        {
          path: "com/airgame/api/airgame-api/2.1.0/airgame-api-2.1.0.pom",
          size: 3584,
          hash: "c8e9be6f03abf532dfbb55ac9d62e271a2c2ed6d8370f6312564a08de86d1afd",
          contentType: "application/xml",
          updatedAt: "2026-01-10T14:33:00Z",
        },
        {
          path: "com/airgame/core/airgame-core/2.1.0/airgame-core-2.1.0.jar",
          size: 8388608,
          hash: "5ad1b0a34bc24762731e9ce37af563add2fefd6fe1215679005e62afb0bd298f",
          contentType: "application/java-archive",
          updatedAt: "2026-01-10T14:34:00Z",
        },
      ],
      "raw-hosted": [
        {
          path: "docs/release-notes-2.4.1.md",
          size: 18432,
          hash: "456da5a4748d3f84166838d9d8d50bb93a5b07018412bc0e2992e7dfc568bb1c",
          contentType: "text/markdown",
          updatedAt: "2026-01-13T10:02:00Z",
        },
        {
          path: "configs/production/app.yaml",
          size: 4096,
          hash: "d0585de88e5814efb63a0852ff0e13be5d8031247ceedb07370b0f123ee4d9dc",
          contentType: "application/yaml",
          updatedAt: "2026-01-16T07:45:00Z",
        },
        {
          path: "artifacts/installer/2.4.1/setup.exe",
          size: 41943040,
          hash: "e148f0e2888d886e29b0ef709a460af9add5f53d46a07bdb01d598de26fb8f88",
          contentType: "application/octet-stream",
          updatedAt: "2026-01-21T09:18:00Z",
        },
        {
          path: "artifacts/installer/2.4.1/setup.exe.sha256",
          size: 64,
          hash: "6d91ed186a9fe2089952f8ac00bed4d1564dd61b5c5172247bc1bfebf7ff5da5",
          contentType: "text/plain",
          updatedAt: "2026-01-21T09:18:00Z",
        },
      ],
    },
    migrations: [],
    publishPolicies: {},
    seq: { user: 5, token: 4, repo: 9, migration: 0, operation: 0 },
    anonymousAccessEnabled: true,
    serviceSettings: {
      publicUrl: "https://repo.example.com",
      upstreamTimeout: 30,
      syncInterval: 5,
      allowedHosts: [],
      originTokenEnabled: false,
      originTokenHeader: "",
      originTokenValue: "",
    },
  };
}

let state: State = seed();

/** 观测数据的快照通道：各端点独立计数，互不干扰。 */
export type SnapshotChannel = "dashboard" | "host" | "download-clients";

/**
 * 快照序号：每次对应端点被请求时递增，驱动一个确定性抖动。
 *
 * 存在的意义：mock 数值若恒定，点页眉刷新后画面没有任何变化，就无法判断刷新是否真的
 * 生效。序号让每次刷新都产生可见的数据变动。
 *
 * **两点约束**：
 * 1. 各端点**独立计数**——同一个页面里先发的列表请求不该吃掉仪表盘的基线序号；
 * 2. **序号 0 是确定性基线快照**，契约测试与前端断言都基于它，因此 `resetStore`
 *    必须把所有通道一并归零，否则同一文件内的后续用例会拿到抖动后的值而断言失败。
 */
const snapshotTicks: Record<SnapshotChannel, number> = {
  dashboard: 0,
  host: 0,
  "download-clients": 0,
};

/** 取某通道的当前快照序号并前移；该通道首次调用恒返回 0（基线快照）。 */
export function nextSnapshotTick(channel: SnapshotChannel): number {
  const current = snapshotTicks[channel];
  snapshotTicks[channel] = current + 1;
  return current;
}

/**
 * 以快照序号为种子的确定性抖动：返回 `[-amplitude, amplitude]` 的整数，序号 0 恒为 0。
 * 用确定性哈希而非 Math.random，保证同一快照序号内多次渲染结果一致（不闪烁）。
 */
export function snapshotJitter(tick: number, salt: number, amplitude: number): number {
  if (tick === 0 || amplitude === 0) return 0;
  const noise = Math.sin(tick * 12.9898 + salt * 78.233) * 43758.5453;
  const unit = noise - Math.floor(noise);
  return Math.round((unit * 2 - 1) * amplitude);
}

/** 重置为初始种子数据；测试用例间隔离状态时调用。 */
export function resetStore(): void {
  state = seed();
  snapshotTicks.dashboard = 0;
  snapshotTicks.host = 0;
  resetObservabilityStore();
  // 种子重建后按当前档位重新物化，避免"档位是大量、store 只剩种子"的不一致。
  reconcileVolumeRepositories(mockVolumeFactor());
}

/** 清空 user 表并复位为未初始化，用于验收“空库自举”路径。 */
export function emptyStore(): void {
  state = seed();
  state.users = [];
  state.tokens = [];
  state.initialized = false;
  state.migrations = [];
  state.seq = { user: 0, token: 0, repo: 3, migration: 0, operation: 0 };
  reconcileVolumeRepositories(mockVolumeFactor());
}

function nowIso(): string {
  return new Date().toISOString();
}

/** 固定生命周期夹具仅服务开发态预览和测试，不改变默认空迁移种子。 */
function migrationLifecycleFixtures(): MigrationTask[] {
  const plan: MigrationPlan = {
    repositories: [{ name: "maven-releases", format: "maven", type: "hosted", estimatedAssets: 2 }],
    warnings: [],
    stats: { repositoryCount: 1, estimatedAssets: 2 },
    estimated: true,
  };
  const createdAt = "2026-08-26T00:00:00Z";
  return [
    {
      id: 1,
      status: "planned",
      sourceType: "online_rest",
      conflictPolicy: "skip",
      createdAt,
      updatedAt: createdAt,
      plan,
    },
    {
      id: 2,
      status: "running",
      sourceType: "offline_dir",
      conflictPolicy: "skip",
      createdAt,
      updatedAt: createdAt,
      startedAt: createdAt,
      plan,
    },
    {
      id: 3,
      status: "failed",
      sourceType: "online_rest",
      conflictPolicy: "skip",
      createdAt,
      updatedAt: createdAt,
      startedAt: createdAt,
      finishedAt: createdAt,
      errorMessage: "迁移执行失败，未完成项可在失败明细中查看",
      plan,
    },
    {
      id: 4,
      status: "cancelled",
      sourceType: "offline_bundle",
      conflictPolicy: "skip",
      createdAt,
      updatedAt: createdAt,
      startedAt: createdAt,
      finishedAt: createdAt,
      errorMessage: "用户取消",
      plan,
    },
    {
      id: 5,
      status: "completed",
      sourceType: "online_rest",
      conflictPolicy: "skip",
      createdAt,
      updatedAt: createdAt,
      startedAt: createdAt,
      finishedAt: createdAt,
      plan,
    },
  ];
}

function pageSlice<T>(items: T[], page: number, pageSize: number): T[] {
  const start = (page - 1) * pageSize;
  return items.slice(start, start + pageSize);
}

type MockAssetOperationTarget = {
  type:
    "raw_path" | "maven_version" | "maven_artifact" | "npm_package" | "npm_version" | "asset_path";
  path: string;
};

type MockAssetOperation = {
  action: "delete" | "move" | "rename";
  targets: MockAssetOperationTarget[];
  destinationPath?: string;
  newPath?: string;
};

function normalizedPath(path: string): string {
  return path.replace(/^\/+|\/+$/g, "");
}

/** 转义正则元字符：仓库名允许 `+.` 等字符，拼进 RegExp 前必须转义。 */
function escapeRegExp(value: string): string {
  return value.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
}

const accessRank = { read: 0, write: 1, admin: 2 } as const;

function canAccessRepository(
  name: string,
  subjectId: number,
  action: keyof typeof accessRank,
): boolean | null {
  const repository = state.repositories.find((item) => item.name === name);
  if (!repository) return null;
  if (action === "read" && repository.visibility === "public") {
    return subjectId !== 0 || state.anonymousAccessEnabled;
  }
  if (subjectId === 0) return false;
  const granted = (state.acls[name] ?? []).find((item) => item.subjectId === subjectId);
  return granted ? accessRank[granted.action] >= accessRank[action] : false;
}

/**
 * 数据量档位物化（实现见 `store.reconcileVolumeRepositories` 的说明）：把种子仓库展开成
 * **真实存在**的克隆仓库，让「列表里有的东西」都能被按名接口解析。
 *
 * 触发点只有两个：模块加载时按当前档位对账一次、`subscribeMockConsole` 收到档位变更时对账。
 * 不做在请求里（曾经这么干过）：深链直接进仓库详情页时第一条请求就是按名查询
 * （`/usage`、`/tree`），而列表请求可能还没发出来 → 竞态 404。
 */
function reconcileVolumeRepositories(factor: number): void {
  if (state.volumeFactor === factor) return;
  // 1) 回收上一轮克隆。
  for (const clone of state.volumeClones) {
    state.repositories = state.repositories.filter((r) => r.name !== clone);
    delete state.acls[clone];
    delete state.assets[clone];
    delete state.connStatus[clone];
  }
  state.volumeClones = [];
  state.volumeFactor = factor;
  if (factor <= 1) return;
  // 2) 从当前种子（此刻 repositories 里只剩种子）物化。
  const seeds = [...state.repositories];
  for (let round = 1; round < factor; round += 1) {
    for (const seed of seeds) {
      const name = `${seed.name}-v${round}`;
      state.repositories.push({ ...seed, id: seed.id + round * 1_000_000, name });
      const assets = state.assets[seed.name];
      if (assets) {
        state.assets[name] = assets.map((asset) => ({ ...asset }));
      }
      const conn = state.connStatus[seed.name];
      if (conn) {
        state.connStatus[name] = { ...conn };
      }
      const acl = state.acls[seed.name];
      if (acl) {
        state.acls[name] = acl.map((entry) => ({ ...entry }));
      }
      state.volumeClones.push(name);
    }
  }
}

function targetAssetPaths(assets: AssetSummary[], target: MockAssetOperationTarget): string[] {
  const path = normalizedPath(target.path);
  if (!path) return [];
  switch (target.type) {
    case "asset_path":
      return assets.filter((asset) => asset.path === path).map((asset) => asset.path);
    case "raw_path":
    case "maven_version":
    case "maven_artifact":
    case "npm_package":
    case "npm_version":
      return assets
        .filter((asset) => asset.path === path || asset.path.startsWith(`${path}/`))
        .map((asset) => asset.path);
  }
}

function searchTokens(query: string): string[] {
  return query.match(/(?:[^\s"]+|"[^"]*")+/g) ?? [];
}

/** FR-114：MD-2 状态合并——offline 优先覆盖为 OFFLINE；hosted 不返回；online 的 proxy 返回内存态。 */
function statusFor(repo: Repository): ConnectionStatus | undefined {
  if (repo.type === "hosted") {
    return undefined;
  }
  if (repo.online === false) {
    return { status: "OFFLINE", description: "已手动离线" };
  }
  return state.connStatus[repo.name] ?? { status: "READY", description: "尚未探测" };
}

/** FR-114：为仓库响应附加连接状态（不修改内部数据）。 */
function decorate(repo: Repository): Repository {
  const status = statusFor(repo);
  return status ? { ...repo, connectionStatus: status } : { ...repo };
}

export const store = {
  status(): StatusInfo {
    return {
      version: state.version,
      ready: true,
      initialized: state.initialized,
      migrationVersion: state.migrationVersion,
      userCount: state.users.length,
      bootstrapAllowed: !state.initialized && state.users.length === 0,
    };
  },

  isInitialized(): boolean {
    return state.initialized;
  },

  /** 空库自举：仅在未初始化时创建首个管理员。返回创建的用户或 null（已初始化）。 */
  bootstrap(username: string): User | null {
    if (state.initialized || state.users.length > 0) {
      return null;
    }
    const user: User = {
      id: ++state.seq.user,
      username,
      role: "admin",
      status: "active",
      webLoginDisabled: false,
      createdAt: nowIso(),
    };
    state.users.push(user);
    state.initialized = true;
    return user;
  },

  /** 登录：校验用户名存在且启用即视为成功（口令在 mock 中不校验）。 */
  login(username: string): User | null {
    const user = state.users.find((u) => u.username === username && u.status === "active");
    return user ?? null;
  },

  listUsers(page: number, pageSize: number): { items: User[]; total: number } {
    return { items: pageSlice(state.users, page, pageSize), total: state.users.length };
  },

  findUser(id: number): User | undefined {
    return state.users.find((u) => u.id === id);
  },

  getPublishPolicy(userId: number, repository: string): PublishPolicy | null {
    const user = state.users.find((item) => item.id === userId);
    const repo = state.repositories.find(
      (item) => item.name === repository && item.type === "hosted",
    );
    if (!user || !repo) return null;
    const saved = state.publishPolicies[`${userId}:${repository}`];
    return {
      userId,
      username: user.username,
      webLoginDisabled: user.webLoginDisabled,
      repository,
      allowedPrefixes: saved?.allowedPrefixes ?? [],
      maxAssetsHour: saved?.maxAssetsHour ?? 0,
      maxBytesDay: saved?.maxBytesDay ?? 0,
      maxFileBytes: saved?.maxFileBytes ?? 0,
      immutableRelease: saved?.immutableRelease ?? false,
    };
  },

  setPublishPolicy(
    userId: number,
    repository: string,
    patch: Omit<PublishPolicy, "userId" | "username" | "repository">,
  ): PublishPolicy | null {
    const user = state.users.find((item) => item.id === userId);
    if (
      !user ||
      !state.repositories.some((item) => item.name === repository && item.type === "hosted")
    ) {
      return null;
    }
    user.webLoginDisabled = patch.webLoginDisabled;
    state.publishPolicies[`${userId}:${repository}`] = {
      webLoginDisabled: patch.webLoginDisabled,
      allowedPrefixes: [...patch.allowedPrefixes],
      maxAssetsHour: patch.maxAssetsHour,
      maxBytesDay: patch.maxBytesDay,
      maxFileBytes: patch.maxFileBytes,
      immutableRelease: patch.immutableRelease,
    };
    return this.getPublishPolicy(userId, repository);
  },

  createUser(username: string, role: User["role"]): User | null {
    if (state.users.some((u) => u.username === username)) {
      return null;
    }
    const user: User = {
      id: ++state.seq.user,
      username,
      role,
      status: "active",
      webLoginDisabled: false,
      createdAt: nowIso(),
    };
    state.users.push(user);
    return user;
  },

  updateUser(
    id: number,
    patch: Partial<Pick<User, "role" | "status" | "webLoginDisabled">>,
  ): User | null {
    const user = state.users.find((u) => u.id === id);
    if (!user) {
      return null;
    }
    if (patch.role) {
      user.role = patch.role;
    }
    if (patch.status) {
      user.status = patch.status;
    }
    if (patch.webLoginDisabled !== undefined) {
      user.webLoginDisabled = patch.webLoginDisabled;
    }
    return user;
  },

  deleteUser(id: number): boolean {
    const before = state.users.length;
    state.users = state.users.filter((u) => u.id !== id);
    return state.users.length < before;
  },

  listTokens(ownerId: number): { items: Token[] } {
    // 列表不含明文：仅回显契约字段，规避明文泄漏。
    return {
      items: state.tokens
        .filter((token) => token.ownerId === ownerId)
        .map((token) => ({
          id: token.id,
          name: token.name,
          createdAt: token.createdAt,
        })),
    };
  },

  createToken(ownerId: number, name: string): TokenCreated {
    const id = ++state.seq.token;
    const plaintext = `jat_${Math.random().toString(36).slice(2, 12)}`;
    const created: StoredToken = { id, name, createdAt: nowIso(), plaintext, ownerId };
    state.tokens.push(created);
    return { id, name, token: plaintext, createdAt: created.createdAt };
  },

  deleteToken(id: number, ownerId: number): boolean {
    const before = state.tokens.length;
    state.tokens = state.tokens.filter((token) => token.id !== id || token.ownerId !== ownerId);
    return state.tokens.length < before;
  },

  /**
   * 数据量档位：把种子仓库**物化**成真实的克隆仓库（`<name>-v<r>`，id 偏移 `r*1_000_000`），
   * 并复制各自的制品树 / 连接状态 / ACL，使所有按名查询（详情、树、清单、ACL、用量、
   * 上传、清理、删除）都能命中。
   *
   * 为什么不沿用「只放大列表响应」的 `scaleList` 做法：那样列表里会出现 store 中并不存在的
   * 仓库，用户点进去后所有按名接口一律 404「仓库不存在」——本方法就是为了消灭这类假条目。
   *
   * 幂等且可回收：档位不变时直接返回（避免同档重复物化，也不会复活已被删除的克隆）；
   * 档位变化时先按账本回收上一轮克隆，再从当前种子重新物化；factor ≤ 1 即只留种子。
   *
   * 局限（有意为之）：克隆是物化时的一份快照，之后对种子做可见性 / 在线状态 / 内容变更
   * 不会自动传播到已存在的克隆；切换档位会按最新种子重建。
   */
  reconcileVolumeRepositories(factor: number): void {
    reconcileVolumeRepositories(factor);
  },

  listRepositories(page: number, pageSize: number): { items: Repository[]; total: number } {
    return {
      items: pageSlice(state.repositories, page, pageSize).map(decorate),
      total: state.repositories.length,
    };
  },

  listAccessibleRepositories(
    subjectId: number,
    page: number,
    pageSize: number,
  ): { items: Repository[]; total: number } {
    const readable = state.repositories.filter((repository) =>
      canAccessRepository(repository.name, subjectId, "read"),
    );
    return {
      items: pageSlice(readable, page, pageSize).map((repository) => ({ ...repository })),
      total: readable.length,
    };
  },

  /** FR-66：匿名可读仓库列表（mock 近似：public 即匿名可读）。 */
  listAnonymousRepositories(
    page: number,
    pageSize: number,
  ): { items: Repository[]; total: number } {
    const readable = state.repositories.filter((r) => r.visibility === "public");
    return {
      items: pageSlice(readable, page, pageSize).map((repository) => ({ ...repository })),
      total: readable.length,
    };
  },

  /** FR-66：匿名访问全局开关。 */
  anonymousAccess(): boolean {
    return state.anonymousAccessEnabled;
  },

  setAnonymousAccess(enabled: boolean): boolean {
    state.anonymousAccessEnabled = enabled;
    return state.anonymousAccessEnabled;
  },

  /** 读取管理端可编辑的服务设置，不返回部署或同步凭据。 */
  settings(): ServiceSettings {
    return { anonymousAccess: state.anonymousAccessEnabled, ...state.serviceSettings };
  },

  /** 保存管理端服务设置；同步间隔保持只读，不由设置页写入。 */
  updateSettings(patch: Omit<ServiceSettings, "syncInterval">): ServiceSettings {
    state.anonymousAccessEnabled = patch.anonymousAccess;
    state.serviceSettings.publicUrl = patch.publicUrl;
    state.serviceSettings.upstreamTimeout = patch.upstreamTimeout;
    state.serviceSettings.allowedHosts = [...patch.allowedHosts];
    state.serviceSettings.originTokenEnabled = patch.originTokenEnabled;
    state.serviceSettings.originTokenHeader = patch.originTokenHeader;
    state.serviceSettings.originTokenValue = patch.originTokenValue;
    return this.settings();
  },

  findRepository(name: string): Repository | undefined {
    const repo = state.repositories.find((r) => r.name === name);
    return repo ? decorate(repo) : undefined;
  },

  createRepository(
    input: Pick<Repository, "name" | "format" | "type"> & {
      visibility?: Repository["visibility"];
      description?: string;
      remoteUrl?: string;
      members?: string[];
    },
  ): Repository | null {
    if (state.repositories.some((r) => r.name === input.name)) {
      return null;
    }
    const repo: Repository = {
      id: ++state.seq.repo,
      name: input.name,
      format: input.format,
      type: input.type,
      visibility: input.visibility ?? "private",
      online: true,
      createdAt: nowIso(),
    };
    if (input.description) {
      repo.description = input.description;
    }
    if (input.remoteUrl) {
      repo.remoteUrl = input.remoteUrl;
    }
    if (input.members && input.members.length > 0) {
      repo.members = input.members;
    }
    state.repositories.push(repo);
    return decorate(repo);
  },

  updateRepository(
    name: string,
    patch: {
      visibility?: Repository["visibility"];
      description?: string;
      remoteUrl?: string;
      members?: string[];
    },
  ): Repository | null {
    const repo = state.repositories.find((r) => r.name === name);
    if (!repo) {
      return null;
    }
    if (patch.visibility) {
      repo.visibility = patch.visibility;
    }
    // FR-81：description 传空串表示清空。
    if (patch.description !== undefined) {
      repo.description = patch.description;
    }
    if (patch.remoteUrl !== undefined) {
      repo.remoteUrl = patch.remoteUrl;
    }
    if (patch.members !== undefined) {
      repo.members = patch.members;
    }
    return decorate(repo);
  },

  /** FR-113：设置仓库 online/offline（节点本地运维状态，不参与复制）。 */
  setOnline(name: string, online: boolean): Repository | null {
    const repo = state.repositories.find((r) => r.name === name);
    if (!repo) {
      return null;
    }
    repo.online = online;
    return decorate(repo);
  },

  /** FR-114：手动重测 proxy 仓库上游连接（仅 online proxy）。mock 视上游恒可达，
   *  重测后置为 AVAILABLE 并返回最新状态；非 proxy/offline 返回 null。 */
  recheckConnection(name: string): ConnectionStatus | null {
    const repo = state.repositories.find((r) => r.name === name);
    if (!repo || repo.type !== "proxy" || repo.online === false) {
      return null;
    }
    const status: ConnectionStatus = { status: "AVAILABLE", description: "上游可用" };
    state.connStatus[name] = status;
    return status;
  },

  deleteRepository(name: string): boolean {
    const before = state.repositories.length;
    // 档位克隆（`<name>-v<r>`）是种子的派生物：种子删掉后它们必须一起消失，
    // 否则列表里会留下点击即 404 的孤儿。
    const clonePattern = new RegExp(`^${escapeRegExp(name)}-v\\d+$`);
    const doomed = state.repositories
      .filter((r) => r.name === name || clonePattern.test(r.name))
      .map((r) => r.name);
    state.repositories = state.repositories.filter((r) => !doomed.includes(r.name));
    for (const doomedName of doomed) {
      delete state.acls[doomedName];
      delete state.assets[doomedName];
      delete state.connStatus[doomedName];
    }
    state.volumeClones = state.volumeClones.filter((clone) => !doomed.includes(clone));
    return state.repositories.length < before;
  },

  getAcl(name: string): AclEntry[] | null {
    if (!state.repositories.some((r) => r.name === name)) {
      return null;
    }
    return state.acls[name] ?? [];
  },

  setAcl(name: string, items: AclEntry[]): AclEntry[] | null {
    if (!state.repositories.some((r) => r.name === name)) {
      return null;
    }
    state.acls[name] = items;
    return items;
  },

  canAccess(name: string, subjectId: number, action: keyof typeof accessRank): boolean | null {
    return canAccessRepository(name, subjectId, action);
  },

  readableRepositoryNames(subjectId: number): string[] {
    return state.repositories
      .filter((repository) => canAccessRepository(repository.name, subjectId, "read"))
      .map((repository) => repository.name);
  },

  /** 列出仓库制品（分页 + 可选路径前缀过滤），仓库不存在返回 null。 */
  listAssets(name: string, prefix: string, page: number, pageSize: number): AssetList | null {
    if (!state.repositories.some((r) => r.name === name)) {
      return null;
    }
    const all = (state.assets[name] ?? [])
      .filter((a) => (prefix ? a.path.startsWith(prefix) : true))
      .sort((a, b) => a.path.localeCompare(b.path));
    return { items: pageSlice(all, page, pageSize), total: all.length };
  },

  /** FR-54：按目录懒加载——返回指定前缀下当前层的目录（全路径带尾斜杠）与文件，仓库不存在返回 null。 */
  listDirectory(
    name: string,
    prefix: string,
  ): { directories: string[]; files: AssetSummary[] } | null {
    if (!state.repositories.some((r) => r.name === name)) {
      return null;
    }
    const dirs = new Set<string>();
    const files: AssetSummary[] = [];
    for (const asset of state.assets[name] ?? []) {
      if (prefix && !asset.path.startsWith(prefix)) {
        continue;
      }
      const rest = asset.path.slice(prefix.length);
      const slash = rest.indexOf("/");
      if (slash >= 0) {
        dirs.add(prefix + rest.slice(0, slash + 1));
      } else {
        files.push(asset);
      }
    }
    return {
      directories: [...dirs].sort(),
      files: files.sort((a, b) => a.path.localeCompare(b.path)),
    };
  },

  /** 开发态 Raw 协议上传：仅登记树展示所需的制品摘要。 */
  putRawAsset(
    name: string,
    path: string,
    size: number,
    contentType: string,
  ): AssetSummary | "conflict" | null {
    const repo = state.repositories.find((item) => item.name === name);
    const normalized = normalizedPath(path);
    if (!repo || !normalized) {
      return null;
    }
    if (repo.format !== "raw" || repo.type !== "hosted") {
      return "conflict";
    }
    const asset: AssetSummary = {
      path: normalized,
      size,
      hash: `mock-${normalized.replaceAll("/", "-")}`,
      contentType: contentType || "application/octet-stream",
      updatedAt: nowIso(),
    };
    const assets = (state.assets[name] ??= []);
    const existing = assets.findIndex((item) => item.path === normalized);
    if (existing >= 0) {
      assets[existing] = asset;
    } else {
      assets.push(asset);
    }
    return asset;
  },

  /** 开发态 Raw 协议删除：仅支持 Raw hosted 的单一路径。 */
  deleteRawAsset(name: string, path: string): boolean | "conflict" | null {
    const repo = state.repositories.find((item) => item.name === name);
    const normalized = normalizedPath(path);
    if (!repo || !normalized) {
      return null;
    }
    if (repo.format !== "raw" || repo.type !== "hosted") {
      return "conflict";
    }
    const assets = state.assets[name] ?? [];
    const index = assets.findIndex((item) => item.path === normalized);
    if (index < 0) {
      return false;
    }
    assets.splice(index, 1);
    return true;
  },

  /** 开发态统一资产操作：先在副本上规划，全部通过后一次替换内存态。 */
  applyAssetOperation(
    name: string,
    operation: MockAssetOperation,
  ): { operationId: string; affected: number } | "conflict" | "invalid" | null {
    const repo = state.repositories.find((item) => item.name === name);
    if (!repo) {
      return null;
    }
    if (
      repo.type !== "hosted" ||
      operation.targets.length === 0 ||
      operation.targets.length > 500
    ) {
      return "invalid";
    }
    const assets = state.assets[name] ?? [];
    const selected = new Set<string>();
    for (const target of operation.targets) {
      const paths = targetAssetPaths(assets, target);
      if (paths.length === 0) {
        return "conflict";
      }
      for (const path of paths) {
        selected.add(path);
      }
    }
    if (selected.size === 0) {
      return "conflict";
    }
    if (selected.size > 500) {
      return "invalid";
    }
    if (operation.action === "delete") {
      state.assets[name] = assets.filter((asset) => !selected.has(asset.path));
      return { operationId: `mock-operation-${++state.seq.operation}`, affected: selected.size };
    }
    if (repo.format !== "raw" || operation.targets.some((target) => target.type !== "raw_path")) {
      return "invalid";
    }
    if (operation.action === "rename" && operation.targets.length !== 1) {
      return "invalid";
    }
    const destination = normalizedPath(
      operation.action === "move" ? (operation.destinationPath ?? "") : (operation.newPath ?? ""),
    );
    if (!destination) {
      return "invalid";
    }
    const replacements = new Map<string, string>();
    for (const target of operation.targets) {
      const sourcePath = normalizedPath(target.path);
      const targetPaths = targetAssetPaths(assets, target);
      const directoryTarget = targetPaths.some((path) => path.startsWith(`${sourcePath}/`));
      for (const path of targetPaths) {
        const next =
          operation.action === "rename"
            ? directoryTarget
              ? `${destination}/${path.slice(sourcePath.length + 1)}`
              : destination
            : directoryTarget
              ? `${destination}/${sourcePath}/${path.slice(sourcePath.length + 1)}`
              : `${destination}/${path.slice(path.lastIndexOf("/") + 1)}`;
        replacements.set(path, next);
      }
    }
    const occupied = new Set(
      assets.filter((asset) => !selected.has(asset.path)).map((asset) => asset.path),
    );
    const replacementPaths = [...replacements.values()];
    if (
      new Set(replacementPaths).size !== replacementPaths.length ||
      replacementPaths.some((path) => occupied.has(path))
    ) {
      return "conflict";
    }
    state.assets[name] = assets.map((asset) => {
      const replacement = replacements.get(asset.path);
      return replacement ? { ...asset, path: replacement, updatedAt: nowIso() } : asset;
    });
    return { operationId: `mock-operation-${++state.seq.operation}`, affected: selected.size };
  },

  /** 开发态 Maven 清理：只移除无 Jar 的版本目录，保留元数据目录。 */
  cleanupEmptyMavenArtifacts(name: string): number | "conflict" | null {
    const repo = state.repositories.find((item) => item.name === name);
    if (!repo) {
      return null;
    }
    if (repo.format !== "maven" || repo.type !== "hosted") {
      return "conflict";
    }
    const assets = state.assets[name] ?? [];
    const versions = new Map<string, AssetSummary[]>();
    for (const asset of assets) {
      if (asset.path.includes("maven-metadata.xml")) continue;
      const slash = asset.path.lastIndexOf("/");
      if (slash <= 0) continue;
      const versionPath = asset.path.slice(0, slash);
      versions.set(versionPath, [...(versions.get(versionPath) ?? []), asset]);
    }
    const emptyPaths = new Set<string>();
    for (const versionAssets of versions.values()) {
      if (!versionAssets.some((asset) => asset.path.endsWith(".jar"))) {
        for (const asset of versionAssets) emptyPaths.add(asset.path);
      }
    }
    if (emptyPaths.size > 0) {
      state.assets[name] = assets.filter((asset) => !emptyPaths.has(asset.path));
    }
    return emptyPaths.size;
  },

  /** 开发态搜索：复用现有表达式字段，提供路径筛选、排序、分页与仓库聚合。 */
  searchAssets(
    query: string,
    sort: string,
    order: "asc" | "desc",
    page: number,
    pageSize: number,
    allowedRepositories?: readonly string[],
  ): {
    items: { repository: string; path: string; size: number; hash: string; updatedAt: string }[];
    total: number;
    facets: { repository: string; count: number }[];
  } {
    const tokens = searchTokens(query);
    const repos = new Set<string>();
    const excludedRepos = new Set<string>();
    const formats = new Set<string>();
    const extensions = new Set<string>();
    const excludedExtensions = new Set<string>();
    const terms: string[] = [];
    const excludedTerms: string[] = [];
    for (const rawToken of tokens) {
      const token = rawToken.replace(/^"|"$/g, "");
      if (token.startsWith("repo:")) repos.add(token.slice("repo:".length));
      else if (token.startsWith("-repo:")) excludedRepos.add(token.slice("-repo:".length));
      else if (token.startsWith("format:")) formats.add(token.slice("format:".length));
      else if (token.startsWith("ext:")) extensions.add(token.slice("ext:".length));
      else if (token.startsWith("-ext:")) excludedExtensions.add(token.slice("-ext:".length));
      else if (token.startsWith("-")) excludedTerms.push(token.slice(1).toLowerCase());
      else terms.push(token.toLowerCase());
    }
    const allowed = allowedRepositories ? new Set(allowedRepositories) : null;
    const all = state.repositories
      .filter((repo) => !allowed || allowed.has(repo.name))
      .flatMap((repo) =>
        (state.assets[repo.name] ?? []).map((asset) => ({
          repository: repo.name,
          format: repo.format,
          ...asset,
        })),
      );
    const matches = all.filter((asset) => {
      const path = asset.path.toLowerCase();
      const extension = asset.path.split(".").at(-1) ?? "";
      return (
        (repos.size === 0 || repos.has(asset.repository)) &&
        !excludedRepos.has(asset.repository) &&
        (formats.size === 0 || formats.has(asset.format)) &&
        (extensions.size === 0 || extensions.has(extension)) &&
        !excludedExtensions.has(extension) &&
        terms.every((term) => path.includes(term)) &&
        excludedTerms.every((term) => !path.includes(term))
      );
    });
    const facets = new Map<string, number>();
    for (const asset of matches) {
      facets.set(asset.repository, (facets.get(asset.repository) ?? 0) + 1);
    }
    const direction = order === "desc" ? -1 : 1;
    matches.sort((left, right) => {
      const leftValue =
        sort === "name"
          ? (left.path.split("/").at(-1) ?? "")
          : sort === "repo"
            ? left.repository
            : sort === "size"
              ? left.size
              : sort === "updated"
                ? left.updatedAt
                : left.path;
      const rightValue =
        sort === "name"
          ? (right.path.split("/").at(-1) ?? "")
          : sort === "repo"
            ? right.repository
            : sort === "size"
              ? right.size
              : sort === "updated"
                ? right.updatedAt
                : right.path;
      return typeof leftValue === "number" && typeof rightValue === "number"
        ? (leftValue - rightValue) * direction
        : String(leftValue).localeCompare(String(rightValue)) * direction;
    });
    return {
      items: pageSlice(matches, page, pageSize).map(
        ({ repository, path, size, hash, updatedAt }) => ({
          repository,
          path,
          size,
          hash,
          updatedAt,
        }),
      ),
      total: matches.length,
      facets: [...facets.entries()]
        .map(([repository, count]) => ({ repository, count }))
        .sort(
          (left, right) =>
            right.count - left.count || left.repository.localeCompare(right.repository),
        ),
    };
  },

  /** FR-73：Maven 网页上传——登记主文件/pom/metadata 及各自校验和的 asset 摘要。
   * 仓库不存在返回 null；非 maven hosted 返回 "conflict"。 */
  uploadMavenArtifact(
    name: string,
    groupId: string,
    artifactId: string,
    version: string,
    packaging: string,
    size: number,
  ):
    | { repository: string; groupId: string; artifactId: string; version: string; files: string[] }
    | "conflict"
    | null {
    const repo = state.repositories.find((r) => r.name === name);
    if (!repo) {
      return null;
    }
    if (repo.format !== "maven" || repo.type !== "hosted") {
      return "conflict";
    }
    const artifactDir = `${groupId.replaceAll(".", "/")}/${artifactId}`;
    const versionDir = `${artifactDir}/${version}`;
    const bases: { path: string; size: number; contentType: string }[] = [
      {
        path: `${versionDir}/${artifactId}-${version}.${packaging}`,
        size,
        contentType: "application/java-archive",
      },
    ];
    if (packaging !== "pom") {
      bases.push({
        path: `${versionDir}/${artifactId}-${version}.pom`,
        size: 512,
        contentType: "application/xml",
      });
    }
    bases.push({
      path: `${artifactDir}/maven-metadata.xml`,
      size: 256,
      contentType: "application/xml",
    });

    const now = new Date().toISOString();
    const list = (state.assets[name] ??= []);
    const files: string[] = [];
    for (const base of bases) {
      for (const entry of [
        base,
        { path: `${base.path}.md5`, size: 32, contentType: "text/plain" },
        { path: `${base.path}.sha1`, size: 40, contentType: "text/plain" },
      ]) {
        const summary: AssetSummary = {
          path: entry.path,
          size: entry.size,
          hash: "f0e1d2c3b4a5f0e1d2c3b4a5f0e1d2c3b4a5f0e1d2c3b4a5f0e1d2c3b4a5f0e1",
          contentType: entry.contentType,
          updatedAt: now,
        };
        const idx = list.findIndex((a) => a.path === entry.path);
        if (idx >= 0) {
          list[idx] = summary;
        } else {
          list.push(summary);
        }
        files.push(entry.path);
      }
    }
    return { repository: name, groupId, artifactId, version, files };
  },

  /** 据仓库 format/type 与对外基址组装客户端接入片段，仓库不存在返回 null。 */
  usage(name: string, baseURL: string): UsageInfo | null {
    const repo = state.repositories.find((r) => r.name === name);
    if (!repo) {
      return null;
    }
    // FR-81：usage 带出描述，供匿名详情页页头展示。
    return {
      format: repo.format,
      type: repo.type,
      ...(repo.description ? { description: repo.description } : {}),
      snippets: buildUsage(repo, baseURL),
    };
  },

  listMigrations(page: number, pageSize: number): { items: MigrationTask[]; total: number } {
    const sorted = [...state.migrations].sort((a, b) => b.id - a.id);
    return { items: pageSlice(sorted, page, pageSize), total: state.migrations.length };
  },

  /** 装载完整生命周期预览夹具，供开发态手动验收与页面测试调用。 */
  seedMigrationLifecycleFixtures(): void {
    state.migrations = migrationLifecycleFixtures();
    state.seq.migration = state.migrations.length;
  },

  /** normal 场景首次读取时提供固定任务，后续写操作保持同一内存态。 */
  ensureMigrationLifecycleFixtures(): void {
    if (state.migrations.length === 0) {
      this.seedMigrationLifecycleFixtures();
    }
  },

  findMigration(id: number): MigrationTask | undefined {
    return state.migrations.find((m) => m.id === id);
  },

  createMigration(input: {
    sourceType: MigrationTask["sourceType"];
    sourceConfig?: MigrationSourceConfig;
    sourceAuth?: MigrationSourceAuth;
    credentialRef?: string;
    conflictPolicy?: MigrationTask["conflictPolicy"];
    plan?: MigrationPlan;
  }): MigrationTask {
    const now = nowIso();
    const task: MigrationTask = {
      id: ++state.seq.migration,
      status: "planned",
      sourceType: input.sourceType,
      conflictPolicy: input.conflictPolicy ?? "skip",
      createdAt: now,
      updatedAt: now,
    };
    const sourceConfig = safeMigrationSourceConfig(input.sourceConfig);
    if (sourceConfig) {
      task.sourceConfig = sourceConfig;
    }
    const sourceAuthType = migrationSourceAuthType(input.sourceAuth);
    if (sourceAuthType) {
      task.sourceAuthType = sourceAuthType;
    }
    if (input.credentialRef) {
      task.credentialRef = input.credentialRef;
    }
    if (input.plan) {
      task.plan = input.plan;
    }
    state.migrations.push(task);
    return task;
  },

  startMigration(
    id: number,
    includeRepositories?: string[],
  ): MigrationTask | "not_found" | "conflict" {
    const task = state.migrations.find((m) => m.id === id);
    if (!task) {
      return "not_found";
    }
    if (task.status !== "planned") {
      return "conflict";
    }
    // 可选收窄 plan
    if (includeRepositories && includeRepositories.length > 0 && task.plan) {
      const allow = new Set(includeRepositories);
      task.plan = {
        ...task.plan,
        repositories: task.plan.repositories.filter((r) => allow.has(r.name)),
      };
    }
    task.status = "running";
    task.startedAt = nowIso();
    task.updatedAt = nowIso();
    return task;
  },

  resumeMigration(id: number): MigrationTask | "not_found" | "conflict" {
    const task = state.migrations.find((m) => m.id === id);
    if (!task) {
      return "not_found";
    }
    if (task.status !== "failed" && task.status !== "cancelled") {
      return "conflict";
    }
    task.status = "running";
    task.startedAt = task.startedAt ?? nowIso();
    task.updatedAt = nowIso();
    delete task.errorMessage;
    return task;
  },

  cancelMigration(id: number): MigrationTask | "not_found" | "conflict" {
    const task = state.migrations.find((m) => m.id === id);
    if (!task) {
      return "not_found";
    }
    if (task.status !== "planned" && task.status !== "running") {
      return "conflict";
    }
    task.status = "cancelled";
    task.finishedAt = nowIso();
    task.updatedAt = nowIso();
    task.errorMessage = "用户取消";
    return task;
  },

  migrationReport(id: number): MigrationReport | null {
    const task = state.migrations.find((m) => m.id === id);
    if (!task) {
      return null;
    }
    const failed = task.status === "failed";
    const completed = task.status === "completed";
    return {
      taskId: task.id,
      status: task.status,
      sourceType: task.sourceType,
      conflictPolicy: task.conflictPolicy,
      startedAt: task.startedAt,
      finishedAt: task.finishedAt,
      totals: {
        copied: completed ? 2 : failed ? 1 : 0,
        skipped: 0,
        failed: failed ? 1 : 0,
        ...(task.status === "running" ? { found: 2, processed: 1, total: 2, percent: 50 } : {}),
      },
      ...(failed
        ? {
            failures: [
              {
                repo: "maven-releases",
                path: "com/example/app/1.0.0/app-1.0.0.jar",
                error: "制品校验失败",
              },
            ],
          }
        : {}),
      cutover: {
        checklist: [
          "将 CI / 客户端 registry 指向本 JianArtifact 实例",
          "将源 Nexus 置为只读（或断开写入）",
          "执行 finalize 增量补齐切换窗口新增制品",
        ],
        delta: null,
      },
      raw: {},
    };
  },

  finalizeMigration(id: number): MigrationTask | "not_found" | "conflict" {
    const task = state.migrations.find((m) => m.id === id);
    if (!task) {
      return "not_found";
    }
    if (task.status !== "completed") {
      return "conflict";
    }
    task.updatedAt = nowIso();
    return task;
  },

  /** discover：同步假计划并落库 planned。 */
  discoverMigration(input: {
    sourceType: MigrationTask["sourceType"];
    sourceConfig?: MigrationSourceConfig;
    sourceAuth?: MigrationSourceAuth;
    credentialRef?: string;
    conflictPolicy?: MigrationTask["conflictPolicy"];
  }): { taskId: number; plan: MigrationPlan } {
    const plan: MigrationPlan = {
      repositories: [
        { name: "maven-releases", format: "maven", type: "hosted", estimatedAssets: 2 },
        { name: "npm-hosted", format: "npm", type: "hosted", estimatedAssets: 1 },
      ],
      warnings: ["docker 仓库已忽略"],
      stats: { repositoryCount: 2, estimatedAssets: 3 },
      estimated: true,
    };
    const task = this.createMigration({
      sourceType: input.sourceType,
      sourceConfig: input.sourceConfig,
      sourceAuth: input.sourceAuth,
      credentialRef: input.credentialRef,
      conflictPolicy: input.conflictPolicy,
      plan,
    });
    return { taskId: task.id, plan };
  },
};

function safeMigrationSourceConfig(input: unknown): MigrationSourceConfig | undefined {
  if (!input || typeof input !== "object") {
    return undefined;
  }
  const config = input as Record<string, unknown>;
  if (typeof config.url === "string") {
    return { url: config.url };
  }
  if (typeof config.sourceRef === "string") {
    return { sourceRef: config.sourceRef };
  }
  if (typeof config.path === "string") {
    return { path: config.path };
  }
  return undefined;
}

function migrationSourceAuthType(input: unknown): MigrationSourceAuthType | undefined {
  if (!input || typeof input !== "object") {
    return undefined;
  }
  const type = (input as { type?: unknown }).type;
  return type === "anonymous" || type === "basic" || type === "bearer" ? type : undefined;
}

/** buildUsage 依仓库 format/type 与对外基址组装接入片段（与后端 domain 层一致）。 */
function buildUsage(repo: Repository, base: string): UsageSnippet[] {
  const writable = repo.type === "hosted";
  const repoURL = `${base}/repository/${repo.name}`;
  if (repo.format === "maven") {
    const snippets: UsageSnippet[] = [
      {
        title: "认证（~/.m2/settings.xml）",
        description: "在 <servers> 中配置凭据。",
        code: `<server>\n  <id>${repo.name}</id>\n  <username><user></username>\n  <password><token></password>\n</server>`,
      },
      {
        title: "解析依赖（pom.xml）",
        description: "在 <repositories> 中声明该仓库。",
        code: `<repository>\n  <id>${repo.name}</id>\n  <url>${repoURL}</url>\n</repository>`,
      },
    ];
    if (writable) {
      snippets.push({
        title: "发布制品（pom.xml + mvn deploy）",
        description: "在 <distributionManagement> 声明部署目标（仅 hosted 可写）。",
        code: `<distributionManagement>\n  <repository>\n    <id>${repo.name}</id>\n    <url>${repoURL}</url>\n  </repository>\n</distributionManagement>`,
      });
    }
    return snippets;
  }
  if (repo.format === "npm") {
    const registryURL = `${base}/npm/${repo.name}/`;
    const snippets: UsageSnippet[] = [
      {
        title: "配置 registry",
        description: "将该仓库设为 npm registry。",
        code: `npm config set registry ${registryURL}`,
      },
      {
        title: "安装依赖",
        description: "从该 registry 安装包。",
        code: `npm install <package> --registry ${registryURL}`,
      },
    ];
    if (writable) {
      snippets.push({
        title: "发布包（npm publish）",
        description: "发布到该仓库（仅 hosted 可写）。",
        code: `npm publish --registry ${registryURL}`,
      });
    }
    return snippets;
  }
  const snippets: UsageSnippet[] = [
    {
      title: "下载制品（curl）",
      description: "以 API Token 作口令（公开仓库可匿名读）。",
      code: `curl -u <user>:<token> -O ${repoURL}/path/to/artifact`,
    },
  ];
  if (writable) {
    snippets.push({
      title: "上传制品（curl）",
      description: "PUT 上传到指定路径（仅 hosted 可写）。",
      code: `curl -u <user>:<token> --upload-file ./artifact ${repoURL}/path/to/artifact`,
    });
  }
  return snippets;
}

// —— 档位物化：模块加载即按当前档位对账，并订阅档位变更 ——
// 必须发生在「第一条请求之前」而不是「列表请求里」：深链直接进仓库详情页时，
// 第一个打到后端的请求可能是 `/usage` 或 `/tree`（按名查询），等列表请求触发就会竞态
// 404。档位不变时 reconcile 直接返回，零开销。
reconcileVolumeRepositories(mockVolumeFactor());
subscribeMockConsole(() => {
  reconcileVolumeRepositories(mockVolumeFactor());
});

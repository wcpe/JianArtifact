// API 领域类型：复用 devmock 生成的 schema.gen.ts（与 api/openapi.yaml 同源），
// 仅做类型级 import（构建期擦除，不把 devmock 运行时打进生产包）。
import type { components } from "@jianartifact/devmock/schema";

type Schemas = components["schemas"];

export type StatusInfo = Schemas["StatusInfo"];
export type EnabledFormats = Schemas["EnabledFormats"];
export type User = Schemas["User"];
export type UserList = Schemas["UserList"];
export type Token = Schemas["Token"];
export type TokenList = Schemas["TokenList"];
export type TokenCreated = Schemas["TokenCreated"];
export type Repository = Schemas["Repository"];
export type RepositoryList = Schemas["RepositoryList"];
export type ConnectionStatus = Schemas["ConnectionStatus"];
export type ConnectionStatusValue = ConnectionStatus["status"];
export type AclEntry = Schemas["AclEntry"];
export type AclList = Schemas["AclList"];
export type LoginResponse = Schemas["LoginResponse"];
export type AssetSummary = Schemas["AssetSummary"];
export type AssetList = Schemas["AssetList"];
export type BatchDeleteAssetsRequest = Schemas["BatchDeleteAssetsRequest"];
export type BatchDeleteAssetFailure = Schemas["BatchDeleteAssetFailure"];
export type BatchDeleteAssetsResponse = Schemas["BatchDeleteAssetsResponse"];
export type UsageSnippet = Schemas["UsageSnippet"];
export type UsageInfo = Schemas["UsageInfo"];
export type MigrationTask = Schemas["MigrationTask"];
export type MigrationTaskList = Schemas["MigrationTaskList"];
// 节点备份与搬迁（FR-132）
export type BackupPackage = Schemas["BackupPackage"];
export type BackupPackageList = Schemas["BackupPackageList"];
export type BackupPackageMode = Schemas["BackupPackage"]["mode"];
export type BackupPackageStatus = Schemas["BackupPackage"]["status"];
export type BackupCounts = Schemas["BackupCounts"];
export type BackupLink = Schemas["BackupLink"];
export type BackupVerification = Schemas["BackupVerification"];
// 写入冻结窗口（FR-134）与从 URL 导入备份包（FR-137）
export type WriteFreezeState = Schemas["WriteFreezeState"];
export type FreezeWritesRequest = Schemas["FreezeWritesRequest"];
export type BackupImport = Schemas["BackupImport"];
export type BackupImportList = Schemas["BackupImportList"];
export type BackupImportStatus = Schemas["BackupImportStatus"];
export type BackupImportOrigin = Schemas["BackupImportOrigin"];
export type CreateBackupImportRequest = Schemas["CreateBackupImportRequest"];
// 分片上传会话（FR-137 第三通道）：init 返回服务端决定的 chunkSize，前端据此切分。
export type BackupUploadSession = Schemas["BackupUploadSession"];
export type BackupUploadStatus = Schemas["BackupUploadStatus"];
export type CreateBackupUploadRequest = Schemas["CreateBackupUploadRequest"];
export type CompleteBackupUploadRequest = Schemas["CompleteBackupUploadRequest"];
export type MigrationPlan = Schemas["MigrationPlan"];
export type MigrationReport = Schemas["MigrationReport"];
export type MigrationDiscoverResponse = Schemas["MigrationDiscoverResponse"];
export type MigrationSourceType = Schemas["MigrationSourceType"];
export type MigrationConflictPolicy = Schemas["MigrationConflictPolicy"];
export type MigrationTaskStatus = Schemas["MigrationTaskStatus"];
export type MigrationSourceAuth = Schemas["MigrationSourceAuth"];
export type MigrationSourceAuthType = Schemas["MigrationSourceAuthType"];
export type RemoteNexusRepositoryRequest = Schemas["RemoteNexusRepositoryRequest"];
export type RemoteNexusRepository = Schemas["RemoteNexusRepository"];
export type RemoteNexusRepositoryList = Schemas["RemoteNexusRepositoryList"];
export type AuditCategory = Schemas["AuditCategory"];
export type AuditResult = Schemas["AuditResult"];
export type AuditAttentionResult = Schemas["AuditAttentionResult"];
export type AuditSeverity = Schemas["AuditSeverity"];
export type AuditAttentionState = Schemas["AuditAttentionState"];
export type AuditTarget = Schemas["AuditTarget"];
export type AuditActorSnapshot = Schemas["AuditActorSnapshot"];
export type AuditAcknowledgement = Schemas["AuditAcknowledgement"];
export type AuditEventAttention = Schemas["AuditEventAttention"];
export type AuditEvent = Schemas["AuditEvent"];
export type AuditEventSafeDetails = Schemas["AuditEventSafeDetails"];
export type AuditEventDetail = Schemas["AuditEventDetail"];
export type AuditCategoryCount = Schemas["AuditCategoryCount"];
export type AuditTrendPoint = Schemas["AuditTrendPoint"];
export type AuditObservabilitySummary = Schemas["AuditObservabilitySummary"];
export type AuditEventPage = Schemas["AuditEventPage"];
export type AuditAttentionPreview = Schemas["AuditAttentionPreview"];
export type AuditAttention = Schemas["AuditAttention"];
export type AuditAttentionPage = Schemas["AuditAttentionPage"];
export type AuditAttentionDetail = Schemas["AuditAttentionDetail"];
export type AcknowledgeAuditAttentionRequest = Schemas["AcknowledgeAuditAttentionRequest"];
export type AcknowledgeAuditAttentionResponse = Schemas["AcknowledgeAuditAttentionResponse"];
export type AuditAttentionNotificationList = Schemas["AuditAttentionNotificationList"];
export type AuditNotificationStatus = Schemas["AuditNotificationStatus"];
export type OperationsDashboard = Schemas["OperationsDashboard"];
export type OperationsAlert = Schemas["OperationsAlert"];
export type HostMonitoring = Schemas["HostMonitoring"];
export type HostMetricGroup = Schemas["HostMetricGroup"];
export type HostMetricPoint = Schemas["HostMetricPoint"];

export type UserRole = User["role"];
export type UserStatus = User["status"];
export type RepoFormat = Repository["format"];
export type RepoType = Repository["type"];
export type RepoVisibility = Repository["visibility"];
export type AclAction = AclEntry["action"];

/** FR-105：统一资产操作 API 的请求目标。 */
export type AssetOperationTargetType =
  "raw_path" | "maven_version" | "maven_artifact" | "npm_package" | "npm_version" | "asset_path";

export interface AssetOperationTarget {
  type: AssetOperationTargetType;
  path: string;
}

export interface AssetOperationInput {
  action: "delete" | "move" | "rename";
  targets: AssetOperationTarget[];
  destinationPath?: string;
  newPath?: string;
  /** 管理员审计原因；未提供时由管理端调用封装填入默认原因。 */
  overrideReason?: string;
}

export interface AssetOperationResponse {
  operationId: string;
  affected: number;
}

// 制品下载计量与分析（FR-142~144）
export type DownloadClientRanking = Schemas["DownloadClientRanking"];

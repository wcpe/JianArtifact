// API 领域类型：复用 devmock 生成的 schema.gen.ts（与 api/openapi.yaml 同源），
// 仅做类型级 import（构建期擦除，不把 devmock 运行时打进生产包）。
import type { components } from "@jianartifact/devmock/schema";

type Schemas = components["schemas"];

export type StatusInfo = Schemas["StatusInfo"];
export type User = Schemas["User"];
export type UserList = Schemas["UserList"];
export type Token = Schemas["Token"];
export type TokenList = Schemas["TokenList"];
export type TokenCreated = Schemas["TokenCreated"];
export type Repository = Schemas["Repository"];
export type RepositoryList = Schemas["RepositoryList"];
export type AclEntry = Schemas["AclEntry"];
export type AclList = Schemas["AclList"];
export type LoginResponse = Schemas["LoginResponse"];
export type AssetSummary = Schemas["AssetSummary"];
export type AssetList = Schemas["AssetList"];
export type UsageSnippet = Schemas["UsageSnippet"];
export type UsageInfo = Schemas["UsageInfo"];

export type UserRole = User["role"];
export type UserStatus = User["status"];
export type RepoFormat = Repository["format"];
export type RepoType = Repository["type"];
export type RepoVisibility = Repository["visibility"];
export type AclAction = AclEntry["action"];

import { Navigate, useParams, useSearchParams } from "react-router-dom";

/** 兼容旧 ACL 地址，实际编辑入口统一收敛到仓库详情页的 ACL 页签。 */
export function AclPage() {
  const { name = "" } = useParams();
  const [searchParams] = useSearchParams();
  const nextParams = new URLSearchParams(searchParams);
  nextParams.set("tab", "acl");

  return (
    <Navigate to={`/repositories/${encodeURIComponent(name)}?${nextParams.toString()}`} replace />
  );
}

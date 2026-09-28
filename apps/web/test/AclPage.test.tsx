import { screen } from "@testing-library/react";
import { Route, Routes, useLocation } from "react-router-dom";
import { describe, expect, it } from "vitest";

import { AclPage } from "../src/pages/AclPage";
import { renderWithProviders } from "./harness";

function LocationProbe() {
  const location = useLocation();
  return <div data-testid="location">{location.pathname + location.search}</div>;
}

function renderAcl(route: string) {
  window.history.replaceState({}, "", route);
  return renderWithProviders(
    <Routes>
      <Route
        path="/repositories/:name/acl"
        element={
          <>
            <AclPage />
            <LocationProbe />
          </>
        }
      />
      <Route path="/repositories/:name" element={<LocationProbe />} />
    </Routes>,
    { route, authenticated: true },
  );
}

describe("仓库访问控制兼容入口", () => {
  it("旧 ACL 路由重定向到详情页 ACL 页签并保留查询参数", async () => {
    renderAcl("/repositories/maven-releases/acl?highlight=com%2Fexample&__mock=empty");

    const location = await screen.findByTestId("location");
    expect(location.textContent).toBe(
      "/repositories/maven-releases?highlight=com%2Fexample&__mock=empty&tab=acl",
    );
  });

  it("旧链接已有 tab 参数时统一切换到 ACL 页签", async () => {
    renderAcl("/repositories/maven-releases/acl?tab=browse");

    expect((await screen.findByTestId("location")).textContent).toBe(
      "/repositories/maven-releases?tab=acl",
    );
  });
});

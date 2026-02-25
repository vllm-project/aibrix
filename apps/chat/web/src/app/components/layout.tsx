import { useState } from "react";
import { Outlet } from "react-router";
import { Sidebar, SidebarToggle } from "./sidebar";

export function Layout() {
  const [sidebarCollapsed, setSidebarCollapsed] = useState(false);

  return (
    <div className="flex h-screen w-screen overflow-hidden bg-background">
      <Sidebar
        collapsed={sidebarCollapsed}
        onToggle={() => setSidebarCollapsed(true)}
      />
      {sidebarCollapsed && (
        <SidebarToggle onClick={() => setSidebarCollapsed(false)} />
      )}
      <div className="flex-1 flex flex-col min-w-0 overflow-hidden">
        <Outlet />
      </div>
    </div>
  );
}

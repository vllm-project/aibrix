import { Outlet } from "react-router";
import Sidebar from "./Sidebar";

export default function AppLayout() {
  return (
    <div className="flex h-screen">
      <Sidebar />
      <main className="flex-1 overflow-y-auto bg-[var(--background)] p-6">
        <Outlet />
      </main>
    </div>
  );
}

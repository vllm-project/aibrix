import { BrowserRouter, Routes, Route } from "react-router";
import AppLayout from "./layout/AppLayout";
import Dashboard from "./pages/Dashboard";
import ModelAdapterList from "./pages/modeladapter/List";
import ModelAdapterCreate from "./pages/modeladapter/Create";
import ModelAdapterDetail from "./pages/modeladapter/Detail";
import RayClusterFleetList from "./pages/rayclusterfleet/List";
import RayClusterFleetCreate from "./pages/rayclusterfleet/Create";
import RayClusterFleetDetail from "./pages/rayclusterfleet/Detail";
import StormServiceList from "./pages/stormservice/List";
import StormServiceCreate from "./pages/stormservice/Create";
import StormServiceDetail from "./pages/stormservice/Detail";
import PodAutoscalerList from "./pages/podautoscaler/List";
import PodAutoscalerCreate from "./pages/podautoscaler/Create";
import PodAutoscalerDetail from "./pages/podautoscaler/Detail";
import KVCacheList from "./pages/kvcache/List";
import KVCacheCreate from "./pages/kvcache/Create";
import KVCacheDetail from "./pages/kvcache/Detail";
import PodSetList from "./pages/podset/List";
import PodSetCreate from "./pages/podset/Create";
import PodSetDetail from "./pages/podset/Detail";

export default function App() {
  return (
    <BrowserRouter>
      <Routes>
        <Route element={<AppLayout />}>
          <Route index element={<Dashboard />} />

          <Route path="modeladapters" element={<ModelAdapterList />} />
          <Route path="modeladapters/create" element={<ModelAdapterCreate />} />
          <Route path="modeladapters/:namespace/:name" element={<ModelAdapterDetail />} />

          <Route path="rayclusterfleets" element={<RayClusterFleetList />} />
          <Route path="rayclusterfleets/create" element={<RayClusterFleetCreate />} />
          <Route path="rayclusterfleets/:namespace/:name" element={<RayClusterFleetDetail />} />

          <Route path="stormservices" element={<StormServiceList />} />
          <Route path="stormservices/create" element={<StormServiceCreate />} />
          <Route path="stormservices/:namespace/:name" element={<StormServiceDetail />} />

          <Route path="podautoscalers" element={<PodAutoscalerList />} />
          <Route path="podautoscalers/create" element={<PodAutoscalerCreate />} />
          <Route path="podautoscalers/:namespace/:name" element={<PodAutoscalerDetail />} />

          <Route path="kvcaches" element={<KVCacheList />} />
          <Route path="kvcaches/create" element={<KVCacheCreate />} />
          <Route path="kvcaches/:namespace/:name" element={<KVCacheDetail />} />

          <Route path="podsets" element={<PodSetList />} />
          <Route path="podsets/create" element={<PodSetCreate />} />
          <Route path="podsets/:namespace/:name" element={<PodSetDetail />} />
        </Route>
      </Routes>
    </BrowserRouter>
  );
}

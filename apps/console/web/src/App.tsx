import { useState } from 'react';
import {
  Navigate,
  Route,
  Routes,
  useLocation,
  useNavigate,
  useParams,
} from 'react-router-dom';
import { Sidebar } from './components/Sidebar';
import { Header } from './components/Header';
import { BatchJobsList } from './components/BatchJobsList';
import { JobDetail } from './components/JobDetail';
import { CreateJob } from './components/CreateJob';
import { Home } from './components/Home';
import { SettingsSidebar } from './components/Settings';
import type { SettingsTab } from './components/Settings';
import { ModelLibrary } from './components/ModelLibrary';
import { ModelDetail } from './components/ModelDetail';
import { CreateModelDeploymentTemplate } from './components/CreateModelDeploymentTemplate';
import { Playground } from './components/Playground';
import { ApiKeysPage } from './components/settings/ApiKeysPage';
import { SecretsPage } from './components/settings/SecretsPage';
import { Toast } from './components/settings/Toast';

// ── Route adapters: each one reads useParams/useNavigate and forwards to the
// existing component. Keeps the leaf components unchanged.

function BatchJobsListRoute() {
  const navigate = useNavigate();
  return (
    <BatchJobsList
      onSelectJob={(id) => navigate(`/batch/${id}`)}
      onCreateJob={() => navigate('/batch/new')}
    />
  );
}

function JobDetailRoute() {
  const { jobId } = useParams<{ jobId: string }>();
  const navigate = useNavigate();
  return <JobDetail jobId={jobId ?? null} onBack={() => navigate('/batch')} />;
}

function CreateJobRoute() {
  const navigate = useNavigate();
  return <CreateJob onBack={() => navigate('/batch')} />;
}

function ModelLibraryRoute() {
  const navigate = useNavigate();
  return <ModelLibrary onSelectModel={(id) => navigate(`/models/${id}`)} />;
}

function ModelDetailRoute() {
  const { modelId } = useParams<{ modelId: string }>();
  const navigate = useNavigate();
  if (!modelId) return <Navigate to="/models" replace />;
  return (
    <ModelDetail
      modelId={modelId}
      onBack={() => navigate('/models')}
      onCreateTemplate={(id) => navigate(`/models/${id}/templates/new`)}
      onEditTemplate={(id, templateId) => navigate(`/models/${id}/templates/${templateId}/edit`)}
      onViewTemplate={(id, templateId) => navigate(`/models/${id}/templates/${templateId}`)}
      onCloneTemplate={(id, templateId) => navigate(`/models/${id}/templates/${templateId}/clone`)}
    />
  );
}

type TemplateRouteMode = 'create' | 'edit' | 'view' | 'clone';

function TemplateFormRoute({ mode }: { mode: TemplateRouteMode }) {
  const { modelId, templateId } = useParams<{ modelId: string; templateId?: string }>();
  const navigate = useNavigate();
  if (!modelId) return <Navigate to="/models" replace />;
  const back = () => navigate(`/models/${modelId}`);
  return (
    <CreateModelDeploymentTemplate
      modelId={modelId}
      templateId={mode === 'edit' || mode === 'view' ? templateId : undefined}
      cloneFromId={mode === 'clone' ? templateId : undefined}
      mode={mode === 'view' ? 'view' : undefined}
      onBack={back}
      onSaved={back}
    />
  );
}

function PlaygroundRoute() {
  const navigate = useNavigate();
  return <Playground onNavigateToModel={(id) => navigate(`/models/${id}`)} />;
}

function ComingSoon({ title, description, features }: {
  title: string;
  description: string;
  features: string[];
}) {
  return (
    <div className="flex-1 flex items-center justify-center p-8">
      <div className="max-w-lg text-center">
        <div className="w-16 h-16 bg-teal-50 rounded-2xl flex items-center justify-center mx-auto mb-6">
          <svg className="w-8 h-8 text-teal-600" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.5" strokeLinecap="round" strokeLinejoin="round">
            <circle cx="12" cy="12" r="10" />
            <path d="M12 6v6l4 2" />
          </svg>
        </div>
        <h2 className="text-xl mb-2 text-gray-900">{title}</h2>
        <p className="text-gray-500 mb-6 leading-relaxed">{description}</p>
        <div className="bg-gray-50 border border-gray-200 rounded-xl p-5 text-left">
          <h3 className="text-sm text-gray-700 mb-3">Planned Features</h3>
          <ul className="space-y-2.5">
            {features.map((f) => (
              <li key={f} className="flex items-start gap-2.5 text-sm text-gray-500">
                <span className="w-1.5 h-1.5 rounded-full bg-teal-400 mt-1.5 flex-shrink-0" />
                {f}
              </li>
            ))}
          </ul>
        </div>
        <div className="mt-6 inline-flex items-center gap-2 px-4 py-2 bg-teal-50 text-teal-700 rounded-full text-sm">
          Coming Soon
        </div>
      </div>
    </div>
  );
}

function DeploymentsComingSoon() {
  return (
    <ComingSoon
      title="Online Deployments"
      description="Spin up dedicated, low-latency endpoints for online inference using a deployment template. The control plane work is still in progress."
      features={[
        'Launch deployments from a registered model + deployment template',
        'Live status, replicas, and accelerator usage per endpoint',
        'Roll, pause, and tear down without leaving the console',
      ]}
    />
  );
}

function LoraComingSoon() {
  return (
    <ComingSoon
      title="LoRA Adapters"
      description="Deploy lightweight LoRA adapters on top of existing base model deployments. Fine-tune model behavior without the cost of full model training or separate deployments."
      features={[
        'Select an existing deployment as the base model',
        'Upload and manage LoRA adapter weights',
        'Hot-swap adapters without restarting the deployment',
        'Monitor adapter-specific performance metrics',
      ]}
    />
  );
}

function SettingsRoute({
  tab,
  showToast,
}: {
  tab: SettingsTab;
  showToast: (message: string, subtitle?: string) => void;
}) {
  switch (tab) {
    case 'api-keys':
      return <ApiKeysPage onToast={showToast} />;
    case 'secrets':
      return <SecretsPage onToast={showToast} />;
  }
}

export default function App() {
  const location = useLocation();
  const navigate = useNavigate();
  const [toast, setToast] = useState<{ message: string; subtitle?: string } | null>(null);

  const showToast = (message: string, subtitle?: string) => {
    setToast({ message, subtitle });
    setTimeout(() => setToast(null), 3000);
  };

  // Settings has its own sidebar; settings tab comes from URL.
  const isSettings = location.pathname.startsWith('/settings');
  const settingsTab: SettingsTab = location.pathname === '/settings/secrets' ? 'secrets' : 'api-keys';
  const isPlayground = location.pathname.startsWith('/playground');

  return (
    <div className="flex h-screen bg-gray-50">
      {isSettings ? (
        <SettingsSidebar
          activeTab={settingsTab}
          onTabChange={(t) => navigate(`/settings/${t}`)}
          onBack={() => navigate(-1)}
        />
      ) : (
        <Sidebar />
      )}
      <div className="flex-1 flex flex-col overflow-hidden">
        <Header />
        <main className={`flex-1 ${isPlayground ? 'overflow-hidden' : 'overflow-y-auto'}`}>
          <Routes>
            <Route path="/" element={<Navigate to="/batch" replace />} />
            <Route path="/home" element={<Home />} />

            <Route path="/batch" element={<BatchJobsListRoute />} />
            <Route path="/batch/new" element={<CreateJobRoute />} />
            <Route path="/batch/:jobId" element={<JobDetailRoute />} />

            <Route path="/playground" element={<PlaygroundRoute />} />

            <Route path="/models" element={<ModelLibraryRoute />} />
            <Route path="/models/:modelId" element={<ModelDetailRoute />} />
            <Route
              path="/models/:modelId/templates/new"
              element={<TemplateFormRoute mode="create" />}
            />
            <Route
              path="/models/:modelId/templates/:templateId"
              element={<TemplateFormRoute mode="view" />}
            />
            <Route
              path="/models/:modelId/templates/:templateId/edit"
              element={<TemplateFormRoute mode="edit" />}
            />
            <Route
              path="/models/:modelId/templates/:templateId/clone"
              element={<TemplateFormRoute mode="clone" />}
            />

            <Route path="/deployments" element={<DeploymentsComingSoon />} />
            <Route path="/lora" element={<LoraComingSoon />} />

            <Route path="/settings" element={<Navigate to="/settings/api-keys" replace />} />
            <Route path="/settings/api-keys" element={<SettingsRoute tab="api-keys" showToast={showToast} />} />
            <Route path="/settings/secrets" element={<SettingsRoute tab="secrets" showToast={showToast} />} />

            <Route path="*" element={<NotFound />} />
          </Routes>
        </main>
      </div>

      {toast && (
        <Toast
          message={toast.message}
          subtitle={toast.subtitle}
          onClose={() => setToast(null)}
        />
      )}
    </div>
  );
}

function NotFound() {
  const location = useLocation();
  return (
    <div className="p-8">
      <h1 className="text-2xl mb-4">Not Found</h1>
      <p className="text-gray-600">No page matches <code className="px-1 py-0.5 bg-gray-100 rounded">{location.pathname}</code>.</p>
    </div>
  );
}

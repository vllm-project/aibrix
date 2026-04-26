import { useState } from 'react';
import { Sidebar } from './components/Sidebar';
import { Header } from './components/Header';
import { BatchJobsList } from './components/BatchJobsList';
import { JobDetail } from './components/JobDetail';
import { CreateJob } from './components/CreateJob';
import { Home } from './components/Home';
import { Deployments } from './components/Deployments';
import { CreateDeployment } from './components/CreateDeployment';
import { DeploymentDetail } from './components/DeploymentDetail';
import { SettingsSidebar } from './components/Settings';
import type { SettingsTab } from './components/Settings';
import { ModelLibrary } from './components/ModelLibrary';
import { ModelDetail } from './components/ModelDetail';
import { CreateModelDeploymentTemplate } from './components/CreateModelDeploymentTemplate';
import { Playground } from './components/Playground';
import { ApiKeysPage } from './components/settings/ApiKeysPage';
import { SecretsPage } from './components/settings/SecretsPage';
import { QuotasPage } from './components/settings/QuotasPage';
import { Toast } from './components/settings/Toast';

export type Page = 'home' | 'batch-jobs' | 'job-detail' | 'create-job' | 'deployments' | 'deployment-detail' | 'create-deployment' | 'settings' | 'model-library' | 'model-detail' | 'model-template-form' | 'playground' | 'lora';

export default function App() {
  const [currentPage, setCurrentPage] = useState<Page>('batch-jobs');
  const [previousPage, setPreviousPage] = useState<Page>('home');
  const [selectedJobId, setSelectedJobId] = useState<string | null>(null);
  const [selectedDeploymentId, setSelectedDeploymentId] = useState<string | null>(null);
  const [selectedModelId, setSelectedModelId] = useState<string | null>(null);
  const [editingTemplateId, setEditingTemplateId] = useState<string | null>(null);
  const [settingsTab, setSettingsTab] = useState<SettingsTab>('api-keys');
  const [toast, setToast] = useState<{ message: string; subtitle?: string } | null>(null);

  const handleNavigate = (page: Page, id?: string) => {
    if (page !== currentPage) {
      setPreviousPage(currentPage);
    }
    setCurrentPage(page);
    if (page === 'job-detail' && id) {
      setSelectedJobId(id);
    } else if (page === 'deployment-detail' && id) {
      setSelectedDeploymentId(id);
    } else if (page === 'model-detail' && id) {
      setSelectedModelId(id);
    }
  };

  const showToast = (message: string, subtitle?: string) => {
    setToast({ message, subtitle });
    setTimeout(() => setToast(null), 3000);
  };

  const renderPage = () => {
    switch (currentPage) {
      case 'home':
        return <Home />;
      case 'batch-jobs':
        return <BatchJobsList onSelectJob={(id) => handleNavigate('job-detail', id)} onCreateJob={() => handleNavigate('create-job')} />;
      case 'job-detail':
        return <JobDetail jobId={selectedJobId} onBack={() => handleNavigate('batch-jobs')} />;
      case 'create-job':
        return <CreateJob onBack={() => handleNavigate('batch-jobs')} />;
      case 'deployments':
        return (
          <Deployments
            onSelectDeployment={(id) => handleNavigate('deployment-detail', id)}
            onCreateDeployment={() => handleNavigate('create-deployment')}
          />
        );
      case 'deployment-detail':
        return <DeploymentDetail deploymentId={selectedDeploymentId} onBack={() => handleNavigate('deployments')} />;
      case 'create-deployment':
        return <CreateDeployment onBack={() => handleNavigate('deployments')} />;
      case 'model-library':
        return <ModelLibrary onSelectModel={(id) => handleNavigate('model-detail', id)} />;
      case 'model-detail':
        return (
          <ModelDetail
            modelId={selectedModelId}
            onBack={() => handleNavigate('model-library')}
            onCreateTemplate={(id) => {
              setSelectedModelId(id);
              setEditingTemplateId(null);
              setPreviousPage('model-detail');
              setCurrentPage('model-template-form');
            }}
            onEditTemplate={(id, templateId) => {
              setSelectedModelId(id);
              setEditingTemplateId(templateId);
              setPreviousPage('model-detail');
              setCurrentPage('model-template-form');
            }}
          />
        );
      case 'model-template-form':
        if (!selectedModelId) {
          return <div className="p-8 text-sm text-gray-500">No model selected.</div>;
        }
        return (
          <CreateModelDeploymentTemplate
            modelId={selectedModelId}
            templateId={editingTemplateId ?? undefined}
            onBack={() => handleNavigate('model-detail', selectedModelId)}
            onSaved={() => handleNavigate('model-detail', selectedModelId)}
          />
        );
      case 'playground':
        return <Playground onNavigateToModel={(id) => handleNavigate('model-detail', id)} />;
      case 'lora':
        return (
          <div className="flex-1 flex items-center justify-center p-8">
            <div className="max-w-lg text-center">
              <div className="w-16 h-16 bg-teal-50 rounded-2xl flex items-center justify-center mx-auto mb-6">
                <svg className="w-8 h-8 text-teal-600" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.5" strokeLinecap="round" strokeLinejoin="round">
                  <path d="M12 3v18" />
                  <path d="M5.5 6.5c2-2 4.5-1.5 6.5 0s4.5 2 6.5 0" />
                  <path d="M5.5 12c2-2 4.5-1.5 6.5 0s4.5 2 6.5 0" />
                  <path d="M5.5 17.5c2-2 4.5-1.5 6.5 0s4.5 2 6.5 0" />
                </svg>
              </div>
              <h2 className="text-xl mb-2 text-gray-900">LoRA Adapters</h2>
              <p className="text-gray-500 mb-6 leading-relaxed">
                Deploy lightweight LoRA adapters on top of existing base model deployments. Fine-tune model behavior without the cost of full model training or separate deployments.
              </p>
              <div className="bg-gray-50 border border-gray-200 rounded-xl p-5 text-left">
                <h3 className="text-sm text-gray-700 mb-3">Planned Features</h3>
                <ul className="space-y-2.5">
                  <li className="flex items-start gap-2.5 text-sm text-gray-500">
                    <span className="w-1.5 h-1.5 rounded-full bg-teal-400 mt-1.5 flex-shrink-0" />
                    Select an existing deployment as the base model
                  </li>
                  <li className="flex items-start gap-2.5 text-sm text-gray-500">
                    <span className="w-1.5 h-1.5 rounded-full bg-teal-400 mt-1.5 flex-shrink-0" />
                    Upload and manage LoRA adapter weights
                  </li>
                  <li className="flex items-start gap-2.5 text-sm text-gray-500">
                    <span className="w-1.5 h-1.5 rounded-full bg-teal-400 mt-1.5 flex-shrink-0" />
                    Hot-swap adapters without restarting the deployment
                  </li>
                  <li className="flex items-start gap-2.5 text-sm text-gray-500">
                    <span className="w-1.5 h-1.5 rounded-full bg-teal-400 mt-1.5 flex-shrink-0" />
                    Monitor adapter-specific performance metrics
                  </li>
                </ul>
              </div>
              <div className="mt-6 inline-flex items-center gap-2 px-4 py-2 bg-teal-50 text-teal-700 rounded-full text-sm">
                <svg className="w-4 h-4" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round">
                  <circle cx="12" cy="12" r="10" />
                  <path d="M12 6v6l4 2" />
                </svg>
                Coming Soon
              </div>
            </div>
          </div>
        );
      case 'settings':
        return renderSettingsContent();
      default:
        return (
          <div className="p-8">
            <h1 className="text-2xl mb-4">{currentPage.charAt(0).toUpperCase() + currentPage.slice(1).replace('-', ' ')}</h1>
            <p className="text-gray-600">This section is coming soon.</p>
          </div>
        );
    }
  };

  const renderSettingsContent = () => {
    switch (settingsTab) {
      case 'api-keys':
        return <ApiKeysPage onToast={showToast} />;
      case 'secrets':
        return <SecretsPage onToast={showToast} />;
      case 'quotas':
        return <QuotasPage />;
    }
  };

  const isSettings = currentPage === 'settings';

  return (
    <div className="flex h-screen bg-gray-50">
      {isSettings ? (
        <SettingsSidebar
          activeTab={settingsTab}
          onTabChange={setSettingsTab}
          onBack={() => handleNavigate(previousPage)}
        />
      ) : (
        <Sidebar currentPage={currentPage} onNavigate={handleNavigate} />
      )}
      <div className="flex-1 flex flex-col overflow-hidden">
        <Header />
        <main className={`flex-1 ${currentPage === 'playground' ? 'overflow-hidden' : 'overflow-y-auto'}`}>
          {renderPage()}
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

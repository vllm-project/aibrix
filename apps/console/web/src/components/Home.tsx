import { useState, useEffect } from 'react';
import { Boxes, Rocket, Library, Gamepad2, ArrowUpRight, Zap, Server } from 'lucide-react';
import { listDeployments, listJobs, listModels } from '../utils/api';

export function Home() {
  const [deploymentCount, setDeploymentCount] = useState<number | null>(null);
  const [jobCount, setJobCount] = useState<number | null>(null);
  const [modelCount, setModelCount] = useState<number | null>(null);

  useEffect(() => {
    listDeployments()
      .then(d => setDeploymentCount(d.length))
      .catch(() => setDeploymentCount(0));

    listJobs()
      .then(res => setJobCount(res.jobs.length))
      .catch(() => setJobCount(0));

    listModels()
      .then(m => setModelCount(m.length))
      .catch(() => setModelCount(0));
  }, []);

  const quickLinks = [
    { icon: Boxes, label: 'Batch Inference', desc: 'Run large-scale inference jobs on datasets', color: 'from-teal-500 to-emerald-600' },
    { icon: Rocket, label: 'Deployments', desc: 'Deploy models with custom scaling options', color: 'from-cyan-500 to-blue-600' },
    { icon: Library, label: 'Model Library', desc: 'Browse open-source models for any task', color: 'from-violet-500 to-purple-600' },
    { icon: Gamepad2, label: 'Playground', desc: 'Test and evaluate models interactively', color: 'from-amber-500 to-orange-600' },
  ];

  return (
    <div className="p-8 max-w-6xl">
      <div className="mb-8">
        <h1 className="text-2xl text-gray-900 mb-1">Welcome back</h1>
        <p className="text-sm text-gray-500">
          Manage your inference workloads, deploy models, and explore the model library.
        </p>
      </div>

      {/* Stats */}
      <div className="grid grid-cols-1 sm:grid-cols-3 gap-4 mb-8">
        <div className="bg-white rounded-xl shadow-sm border border-gray-100 p-5">
          <div className="flex items-center gap-3 mb-3">
            <div className="w-9 h-9 bg-teal-50 rounded-lg flex items-center justify-center">
              <Zap className="w-4 h-4 text-teal-600" />
            </div>
            <span className="text-sm text-gray-500">Active Deployments</span>
          </div>
          <div className="text-2xl text-gray-900">
            {deploymentCount !== null ? deploymentCount : '...'}
          </div>
        </div>
        <div className="bg-white rounded-xl shadow-sm border border-gray-100 p-5">
          <div className="flex items-center gap-3 mb-3">
            <div className="w-9 h-9 bg-blue-50 rounded-lg flex items-center justify-center">
              <Server className="w-4 h-4 text-blue-600" />
            </div>
            <span className="text-sm text-gray-500">Batch Jobs (30d)</span>
          </div>
          <div className="text-2xl text-gray-900">
            {jobCount !== null ? jobCount : '...'}
          </div>
        </div>
        <div className="bg-white rounded-xl shadow-sm border border-gray-100 p-5">
          <div className="flex items-center gap-3 mb-3">
            <div className="w-9 h-9 bg-violet-50 rounded-lg flex items-center justify-center">
              <Library className="w-4 h-4 text-violet-600" />
            </div>
            <span className="text-sm text-gray-500">Available Models</span>
          </div>
          <div className="text-2xl text-gray-900">
            {modelCount !== null ? modelCount : '...'}
          </div>
        </div>
      </div>

      {/* Quick Links */}
      <h2 className="text-lg text-gray-900 mb-4">Quick Links</h2>
      <div className="grid grid-cols-1 md:grid-cols-2 gap-4">
        {quickLinks.map((link) => {
          const Icon = link.icon;
          return (
            <div
              key={link.label}
              className="bg-white rounded-xl shadow-sm border border-gray-100 p-5 cursor-pointer hover:shadow-md transition-all group"
            >
              <div className="flex items-start justify-between">
                <div className="flex items-start gap-4">
                  <div className={`w-10 h-10 bg-gradient-to-br ${link.color} rounded-xl flex items-center justify-center`}>
                    <Icon className="w-5 h-5 text-white" />
                  </div>
                  <div>
                    <h3 className="text-sm text-gray-900 mb-1">{link.label}</h3>
                    <p className="text-xs text-gray-500">{link.desc}</p>
                  </div>
                </div>
                <ArrowUpRight className="w-4 h-4 text-gray-300 group-hover:text-teal-500 transition-colors" />
              </div>
            </div>
          );
        })}
      </div>
    </div>
  );
}

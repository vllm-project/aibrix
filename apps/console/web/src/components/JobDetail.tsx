import { ChevronLeft, Copy, Download, Trash2, CheckCircle, XCircle } from 'lucide-react';
import { mockJobs } from '../data/mockData';

interface JobDetailProps {
  jobId: string | null;
  onBack: () => void;
}

export function JobDetail({ jobId, onBack }: JobDetailProps) {
  const job = mockJobs.find(j => j.id === jobId);
  
  if (!job) {
    return (
      <div className="p-8">
        <button onClick={onBack} className="flex items-center gap-2 text-sm text-gray-500 hover:text-gray-900 mb-4">
          <ChevronLeft className="w-4 h-4" />
          Back
        </button>
        <p>Job not found</p>
      </div>
    );
  }

  const isCompleted = job.status === 'Completed';

  return (
    <div className="p-8">
      <div className="mb-6">
        <button onClick={onBack} className="flex items-center gap-2 text-sm text-gray-500 hover:text-gray-900 mb-4">
          <ChevronLeft className="w-4 h-4" />
          Batch Inference / <span className="text-gray-400">{job.inferenceId}</span>
        </button>
        
        <div className="flex items-center justify-between">
          <div>
            <h1 className="text-2xl mb-2">{job.name}</h1>
            <div className="flex items-center gap-3">
              <code className="text-sm text-gray-600 bg-gray-100 px-2 py-1 rounded-md">
                {job.fullPath}
              </code>
              <button className="text-gray-400 hover:text-gray-600">
                <Copy className="w-4 h-4" />
              </button>
              <span
                className={`inline-flex px-2.5 py-1 text-xs rounded-full ${
                  job.status === 'Completed'
                    ? 'bg-emerald-50 text-emerald-700 border border-emerald-200'
                    : job.status === 'Validating'
                    ? 'bg-amber-50 text-amber-700 border border-amber-200'
                    : 'bg-red-50 text-red-700 border border-red-200'
                }`}
              >
                {job.status}
              </span>
            </div>
          </div>
          
          <button className="w-10 h-10 rounded-lg border border-gray-200 flex items-center justify-center hover:bg-gray-50">
            <Trash2 className="w-4 h-4 text-red-500" />
          </button>
        </div>
      </div>

      <div className="grid grid-cols-1 lg:grid-cols-3 gap-6">
        <div className="lg:col-span-2 space-y-6">
          {!isCompleted && (
            <div className="bg-white rounded-xl shadow-sm border border-gray-100 p-6">
              <div className="flex items-center gap-2 mb-4">
                <div className="w-5 h-5 rounded-full border-2 border-gray-300 border-t-teal-600 animate-spin"></div>
                <span className="text-sm">Validating</span>
              </div>
              
              <div className="relative">
                <div className="h-2 bg-gray-100 rounded-full overflow-hidden">
                  <div className="h-full bg-teal-500 rounded-full" style={{ width: '0%' }}></div>
                </div>
                <div className="text-center mt-2 text-sm text-gray-500">0%</div>
              </div>
              
              <p className="text-center text-sm text-gray-500 mt-4">Batch inference is in progress.</p>
            </div>
          )}

          {isCompleted && (
            <>
              <div className="bg-white rounded-xl shadow-sm border border-gray-100 p-6">
                <h2 className="text-lg mb-4">Processing Summary</h2>
                
                <div className="mb-6">
                  <div className="text-sm text-gray-500 mb-2">Total processed input rows</div>
                  <div className="text-3xl">1,000</div>
                  
                  <div className="grid grid-cols-2 gap-4 mt-4">
                    <div className="flex items-start gap-2">
                      <CheckCircle className="w-5 h-5 text-emerald-500 mt-1" />
                      <div>
                        <div className="text-sm text-gray-500">Succeed</div>
                        <div className="text-xl">1,000</div>
                        <div className="text-sm text-emerald-600">100.00%</div>
                      </div>
                    </div>
                    
                    <div className="flex items-start gap-2">
                      <XCircle className="w-5 h-5 text-red-500 mt-1" />
                      <div>
                        <div className="text-sm text-gray-500">Failed</div>
                        <div className="text-xl">0</div>
                      </div>
                    </div>
                  </div>
                </div>
                
                <div className="border-t border-gray-100 pt-6">
                  <div className="text-sm text-gray-500 mb-2">Successfully output rows</div>
                  <div className="text-3xl">1,000</div>
                </div>
              </div>

              <div className="bg-white rounded-xl shadow-sm border border-gray-100 p-6">
                <h2 className="text-lg mb-4">Token Used</h2>
                
                <div className="grid grid-cols-3 gap-6">
                  <div>
                    <div className="text-sm text-gray-500 mb-2">Total tokens</div>
                    <div className="text-2xl">493,354</div>
                  </div>
                  
                  <div>
                    <div className="text-sm text-gray-500 mb-2">Prompt tokens</div>
                    <div className="text-2xl">476,498</div>
                  </div>
                  
                  <div>
                    <div className="text-sm text-gray-500 mb-2">Completion tokens</div>
                    <div className="text-2xl">16,856</div>
                  </div>
                </div>
              </div>
            </>
          )}
        </div>

        <div className="space-y-6">
          <div className="bg-white rounded-xl shadow-sm border border-gray-100 p-6">
            <h2 className="text-lg mb-4">Info</h2>
            
            <div className="space-y-3 text-sm">
              <div>
                <div className="text-gray-500 mb-1">Created By</div>
                <div className="text-gray-900">{job.createdBy}</div>
              </div>
              
              <div>
                <div className="text-gray-500 mb-1">Created On</div>
                <div className="text-gray-900">{isCompleted ? '17 minutes ago' : '0 minute ago'}</div>
              </div>
              
              <div>
                <div className="text-gray-500 mb-1">Last Updated</div>
                <div className="text-gray-900">{isCompleted ? '7 minutes ago' : '0 minute ago'}</div>
              </div>
              
              <div>
                <div className="text-gray-500 mb-1">Display Name</div>
                <div className="text-gray-900">{job.name}</div>
              </div>
            </div>
          </div>

          <div className="bg-white rounded-xl shadow-sm border border-gray-100 p-6">
            <h2 className="text-lg mb-4">Model and Datasets</h2>
            
            <div className="space-y-3 text-sm">
              <div>
                <div className="text-gray-500 mb-1">Model</div>
                <div className="flex items-center gap-2">
                  <code className="text-sm bg-gray-100 px-2 py-1 rounded-md">{job.modelId}</code>
                  <button className="text-gray-400 hover:text-gray-600">
                    <Copy className="w-3 h-3" />
                  </button>
                </div>
              </div>
              
              <div>
                <div className="text-gray-500 mb-1">Input Dataset</div>
                <div className="flex items-center gap-2">
                  <code className="text-sm bg-gray-100 px-2 py-1 rounded-md">{job.inputDatasetId}</code>
                  <button className="text-gray-400 hover:text-gray-600">
                    <Copy className="w-3 h-3" />
                  </button>
                </div>
              </div>
              
              {isCompleted && (
                <div>
                  <div className="text-gray-500 mb-1">Output Dataset</div>
                  <div className="flex items-center gap-2">
                    <code className="text-sm bg-gray-100 px-2 py-1 rounded-md">bij-d9xwfrf8</code>
                    <button className="text-gray-400 hover:text-gray-600">
                      <Copy className="w-3 h-3" />
                    </button>
                    <button className="text-gray-400 hover:text-gray-600">
                      <Download className="w-3 h-3" />
                    </button>
                  </div>
                </div>
              )}
            </div>
          </div>

          <div className="bg-white rounded-xl shadow-sm border border-gray-100 p-6">
            <h2 className="text-lg mb-4">Inference Parameters</h2>
            
            <div className="space-y-3 text-sm">
              <div className="flex justify-between">
                <span className="text-gray-500">Max Tokens</span>
                <span className="text-gray-900">100</span>
              </div>
              
              <div className="flex justify-between">
                <span className="text-gray-500">Temperature</span>
                <span className="text-gray-900">Not set</span>
              </div>
              
              <div className="flex justify-between">
                <span className="text-gray-500">Top P</span>
                <span className="text-gray-900">Not set</span>
              </div>
              
              <div className="flex justify-between">
                <span className="text-gray-500">N</span>
                <span className="text-gray-900">Not set</span>
              </div>
              
              <div className="flex justify-between">
                <span className="text-gray-500">Top K</span>
                <span className="text-gray-900">Not set</span>
              </div>
            </div>
          </div>
        </div>
      </div>
    </div>
  );
}

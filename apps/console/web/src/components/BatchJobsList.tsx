import { Search, ChevronDown } from 'lucide-react';
import { mockJobs } from '../data/mockData';

interface BatchJobsListProps {
  onSelectJob: (id: string) => void;
  onCreateJob: () => void;
}

export function BatchJobsList({ onSelectJob, onCreateJob }: BatchJobsListProps) {
  return (
    <div className="p-8">
      <div className="mb-6 flex items-start justify-between">
        <div>
          <h1 className="text-2xl mb-2">Batch Inference Jobs</h1>
          <p className="text-sm text-gray-500">View your past batch inference jobs or create new ones.</p>
        </div>
        <button
          onClick={onCreateJob}
          className="px-4 py-2 bg-teal-600 text-white rounded-lg text-sm hover:bg-teal-700 transition-colors"
        >
          Create Batch Inference Job
        </button>
      </div>

      <div className="mb-6 flex items-center gap-4">
        <div className="flex-1 relative">
          <Search className="absolute left-3 top-1/2 transform -translate-y-1/2 w-4 h-4 text-gray-400" />
          <input
            type="text"
            placeholder="Search by id, name, or created by"
            className="w-full pl-10 pr-4 py-2 border border-gray-200 rounded-lg text-sm focus:outline-none focus:ring-2 focus:ring-teal-500/30 focus:border-teal-500 bg-white"
          />
        </div>
        
        <div className="flex items-center gap-2 px-4 py-2 border border-gray-200 rounded-lg text-sm cursor-pointer hover:bg-gray-50 bg-white">
          <span className="text-gray-500">Status:</span>
          <span>All</span>
          <ChevronDown className="w-4 h-4" />
        </div>
      </div>

      <div className="bg-white rounded-xl shadow-sm border border-gray-100 overflow-hidden">
        <div className="overflow-x-auto">
          <table className="w-full">
            <thead className="bg-gray-50/80 border-b border-gray-100">
              <tr>
                <th className="px-6 py-3 text-left text-xs text-gray-500 uppercase tracking-wider">
                  Batch inference jobs
                </th>
                <th className="px-6 py-3 text-left text-xs text-gray-500 uppercase tracking-wider">
                  Model
                </th>
                <th className="px-6 py-3 text-left text-xs text-gray-500 uppercase tracking-wider">
                  Input dataset
                </th>
                <th className="px-6 py-3 text-left text-xs text-gray-500 uppercase tracking-wider">
                  Create time
                </th>
                <th className="px-6 py-3 text-left text-xs text-gray-500 uppercase tracking-wider">
                  Created by
                </th>
                <th className="px-6 py-3 text-left text-xs text-gray-500 uppercase tracking-wider">
                  Status
                </th>
                <th className="px-6 py-3"></th>
              </tr>
            </thead>
            <tbody className="divide-y divide-gray-50">
              {mockJobs.map((job) => (
                <tr
                  key={job.id}
                  className="hover:bg-gray-50/50 cursor-pointer transition-colors"
                  onClick={() => onSelectJob(job.id)}
                >
                  <td className="px-6 py-4">
                    <div className="text-sm text-gray-900">{job.name}</div>
                    <div className="text-xs text-gray-400">ID: {job.inferenceId}</div>
                  </td>
                  <td className="px-6 py-4">
                    <div className="text-sm text-gray-900">{job.model}</div>
                    <div className="text-xs text-gray-400">ID: {job.modelId}</div>
                  </td>
                  <td className="px-6 py-4">
                    <div className="text-sm text-gray-900">{job.inputDataset}</div>
                    <div className="text-xs text-gray-400">ID: {job.inputDatasetId}</div>
                  </td>
                  <td className="px-6 py-4 text-sm text-gray-500">
                    <div>{job.createDate}</div>
                    <div className="text-xs text-gray-400">{job.createTime}</div>
                  </td>
                  <td className="px-6 py-4 text-sm text-gray-500">
                    {job.createdBy}
                  </td>
                  <td className="px-6 py-4">
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
                  </td>
                  <td className="px-6 py-4">
                    <button className="text-gray-300 hover:text-gray-500">
                      <svg className="w-5 h-5" fill="currentColor" viewBox="0 0 24 24">
                        <circle cx="12" cy="5" r="2" />
                        <circle cx="12" cy="12" r="2" />
                        <circle cx="12" cy="19" r="2" />
                      </svg>
                    </button>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      </div>
    </div>
  );
}

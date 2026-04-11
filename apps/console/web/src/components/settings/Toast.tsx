import { CheckCircle, X } from 'lucide-react';

interface ToastProps {
  message: string;
  subtitle?: string;
  onClose: () => void;
}

export function Toast({ message, subtitle, onClose }: ToastProps) {
  return (
    <div className="fixed bottom-6 right-6 z-50 animate-slide-up">
      <div className="bg-gray-800 text-white rounded-xl px-5 py-4 shadow-2xl flex items-start gap-3 min-w-[280px] max-w-[360px]">
        <CheckCircle className="w-5 h-5 text-green-400 flex-shrink-0 mt-0.5" />
        <div className="flex-1 min-w-0">
          <div className="text-sm font-medium">{message}</div>
          {subtitle && (
            <div className="text-xs text-gray-300 mt-0.5">{subtitle}</div>
          )}
        </div>
        <button
          onClick={onClose}
          className="text-gray-400 hover:text-white flex-shrink-0 mt-0.5"
        >
          <X className="w-4 h-4" />
        </button>
      </div>
    </div>
  );
}

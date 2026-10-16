import type {
  CreateJobResourceFieldsProps,
  CreateJobResourcePlugin,
} from './registry';

export const DEFAULT_RESOURCE_DURATION = '1h';

export function durationForCompletionWindow(
  duration: unknown,
  completionWindow: string,
): string {
  const completionHours = parseInt(completionWindow, 10);
  const durationHours = typeof duration === 'string'
    ? parseInt(duration, 10)
    : 1;
  const normalizedHours = Number.isFinite(durationHours)
    ? Math.min(Math.max(durationHours, 1), completionHours)
    : 1;
  return `${normalizedHours}h`;
}

export function DemoRenderer({
  completionWindow,
  value,
  onChange,
}: CreateJobResourceFieldsProps) {
  const duration = durationForCompletionWindow(value.duration, completionWindow);

  return (
    <div>
      <label className="block text-sm mb-1">Duration</label>
      <p className="text-xs text-gray-400 mb-1">
        Continuous resource time required within the completion window.
      </p>
      <select
        value={duration}
        onChange={(event) => onChange({ ...value, duration: event.target.value })}
        className="w-full px-4 py-2 border border-gray-200 rounded-lg text-sm focus:outline-none focus:ring-2 focus:ring-teal-500/30 focus:border-teal-500 bg-white"
      >
        {Array.from(
          { length: parseInt(completionWindow, 10) },
          (_, index) => index + 1,
        ).map(hours => (
          <option key={hours} value={`${hours}h`}>
            {hours} hr
          </option>
        ))}
      </select>
    </div>
  );
}

export function createDemoResourcePlugin(
  Fields: CreateJobResourcePlugin['Fields'],
): CreateJobResourcePlugin {
  return {
    Fields,
    normalize: (value, completionWindow) => ({
      ...value,
      duration: durationForCompletionWindow(value.duration, completionWindow),
    }),
    toProviderConfig: (value, completionWindow) => ({
      duration: durationForCompletionWindow(
        value.duration ?? DEFAULT_RESOURCE_DURATION,
        completionWindow,
      ),
    }),
  };
}

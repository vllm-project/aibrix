export interface BatchLineRecord {
  custom_id?: string;
  method?: string;
  url?: string;
  body?: {
    model?: string;
    [key: string]: unknown;
  };
  [key: string]: unknown;
}

export interface ValidationResult {
  valid: boolean;
  totalLines: number;
  errors: string[];
  warnings: string[];
  detectedModel: string | null;
  endpoints: string[];
}

const MAX_ERRORS = 20;

export async function validateBatchFile(
  file: File,
  expectedModel: string,
): Promise<ValidationResult> {
  const text = await file.text();
  const rawLines = text.trimEnd().split('\n');
  const errors: string[] = [];
  const warnings: string[] = [];
  const models = new Set<string>();
  const endpoints = new Set<string>();

  if (rawLines.length === 0 || (rawLines.length === 1 && rawLines[0].trim() === '')) {
    return { valid: false, totalLines: 0, errors: ['File is empty'], warnings, detectedModel: null, endpoints: [] };
  }

  for (let i = 0; i < rawLines.length; i++) {
    if (errors.length >= MAX_ERRORS) {
      errors.push(`... and more errors (stopped after ${MAX_ERRORS})`);
      break;
    }

    const line = rawLines[i].trim();
    if (line === '') {
      if (i < rawLines.length - 1) {
        warnings.push(`Line ${i + 1}: empty line (will be skipped)`);
      }
      continue;
    }

    let row: BatchLineRecord;
    try {
      row = JSON.parse(line) as BatchLineRecord;
    } catch {
      errors.push(`Line ${i + 1}: invalid JSON`);
      continue;
    }

    if (typeof row !== 'object' || row === null || Array.isArray(row)) {
      errors.push(`Line ${i + 1}: must be a JSON object`);
      continue;
    }

    if (!row.custom_id) {
      errors.push(`Line ${i + 1}: missing "custom_id"`);
    }
    if (!row.method) {
      errors.push(`Line ${i + 1}: missing "method"`);
    }
    if (!row.url) {
      errors.push(`Line ${i + 1}: missing "url"`);
    }
    if (!row.body) {
      errors.push(`Line ${i + 1}: missing "body"`);
    }

    if (row.body?.model) {
      models.add(row.body.model);
    }
    if (row.url) {
      endpoints.add(row.url);
    }
  }

  // Cross-request model consistency
  if (models.size > 1) {
    errors.push(`Mixed models found: ${[...models].join(', ')}. All requests must use the same model.`);
  }

  // Match against selected model from UI
  const detectedModel = models.size === 1 ? [...models][0] : models.size > 1 ? [...models][0] : null;
  if (detectedModel && detectedModel !== expectedModel) {
    errors.push(
      `Model in file "${detectedModel}" doesn't match selected model "${expectedModel}".`,
    );
  }

  const nonEmptyLines = rawLines.filter(l => l.trim() !== '').length;

  return {
    valid: errors.length === 0,
    totalLines: nonEmptyLines,
    errors,
    warnings,
    detectedModel,
    endpoints: [...endpoints],
  };
}

export function generateJobDisplayName(modelName: string): string {
  const now = new Date();
  const ts = now.toISOString().replace(/[-:T]/g, '').slice(0, 14); // 20260306143022
  const short = modelName.replace(/[^a-zA-Z0-9._-]/g, '-').toLowerCase();
  return `${short}-batch-${ts}`;
}

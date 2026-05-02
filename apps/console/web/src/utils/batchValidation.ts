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

export interface ValidationContext {
  expectedModel: string;
  supportedEndpoints?: string[];
}

export interface ParsedLine {
  lineNumber: number;
  record: BatchLineRecord;
}

export interface ParseResult {
  records: ParsedLine[];
  parseErrors: Array<{ lineNumber: number; message: string }>;
  warnings: Array<{ lineNumber: number; message: string }>;
  totalLines: number;
}

export interface BatchOverrides {
  maxTokens?: number;
  temperature?: number;
  topP?: number;
  n?: number;
}

export interface MutationDiff {
  totalLines: number;
  changedLines: number;
  fieldsChanged: Record<string, number>;
  skipped: Array<{ lineNumber: number; field: string; reason: string }>;
  samples: Array<{ lineNumber: number; field: string; from: unknown; to: unknown }>;
}

const MAX_ERRORS = 20;
const MAX_DIFF_SAMPLES = 5;

const OVERRIDE_FIELD_MAP: Record<keyof BatchOverrides, string> = {
  maxTokens: 'max_tokens',
  temperature: 'temperature',
  topP: 'top_p',
  n: 'n',
};

// Endpoints where completion-style sampling params apply.
const COMPLETION_ENDPOINTS = new Set(['/v1/chat/completions', '/v1/completions']);

export function parseJsonl(text: string): ParseResult {
  const rawLines = text.trimEnd().split('\n');
  const records: ParsedLine[] = [];
  const parseErrors: ParseResult['parseErrors'] = [];
  const warnings: ParseResult['warnings'] = [];

  if (rawLines.length === 0 || (rawLines.length === 1 && rawLines[0].trim() === '')) {
    return { records, parseErrors: [{ lineNumber: 1, message: 'File is empty' }], warnings, totalLines: 0 };
  }

  let nonEmpty = 0;
  for (let i = 0; i < rawLines.length; i++) {
    const lineNumber = i + 1;
    const line = rawLines[i].trim();
    if (line === '') {
      if (i < rawLines.length - 1) {
        warnings.push({ lineNumber, message: 'empty line (will be skipped)' });
      }
      continue;
    }
    nonEmpty++;

    let parsed: unknown;
    try {
      parsed = JSON.parse(line);
    } catch {
      parseErrors.push({ lineNumber, message: 'invalid JSON' });
      continue;
    }
    if (typeof parsed !== 'object' || parsed === null || Array.isArray(parsed)) {
      parseErrors.push({ lineNumber, message: 'must be a JSON object' });
      continue;
    }

    records.push({ lineNumber, record: parsed as BatchLineRecord });
  }

  return { records, parseErrors, warnings, totalLines: nonEmpty };
}

export function validateBatchLines(parsed: ParseResult, ctx: ValidationContext): ValidationResult {
  const errors: string[] = [];
  const warnings: string[] = parsed.warnings.map((w) => `Line ${w.lineNumber}: ${w.message}`);
  const models = new Set<string>();
  const endpoints = new Set<string>();
  const supported = ctx.supportedEndpoints && ctx.supportedEndpoints.length > 0
    ? new Set(ctx.supportedEndpoints)
    : null;

  const pushError = (msg: string) => {
    if (errors.length >= MAX_ERRORS) {
      if (!errors[errors.length - 1]?.startsWith('... and more')) {
        errors.push(`... and more errors (stopped after ${MAX_ERRORS})`);
      }
      return;
    }
    errors.push(msg);
  };

  for (const e of parsed.parseErrors) {
    pushError(e.lineNumber === 1 && e.message === 'File is empty'
      ? 'File is empty'
      : `Line ${e.lineNumber}: ${e.message}`);
  }

  for (const { lineNumber, record } of parsed.records) {
    if (errors.length >= MAX_ERRORS) break;

    if (!record.custom_id) pushError(`Line ${lineNumber}: missing "custom_id"`);
    if (!record.method) pushError(`Line ${lineNumber}: missing "method"`);
    if (!record.url) pushError(`Line ${lineNumber}: missing "url"`);
    if (!record.body) pushError(`Line ${lineNumber}: missing "body"`);

    if (record.body?.model) models.add(record.body.model);
    if (record.url) {
      endpoints.add(record.url);
      if (supported && !supported.has(record.url)) {
        pushError(
          `Line ${lineNumber}: url "${record.url}" is not in the deployment template's ` +
          `supported endpoints [${[...supported].join(', ')}]`,
        );
      }
    }
  }

  if (models.size > 1) {
    pushError(`Mixed models found: ${[...models].join(', ')}. All requests must use the same model.`);
  }

  const detectedModel = models.size >= 1 ? [...models][0] : null;
  if (detectedModel && detectedModel !== ctx.expectedModel) {
    pushError(`Model in file "${detectedModel}" doesn't match selected model "${ctx.expectedModel}".`);
  }

  return {
    valid: errors.length === 0,
    totalLines: parsed.totalLines,
    errors,
    warnings,
    detectedModel,
    endpoints: [...endpoints],
  };
}

// Convenience wrapper: read file, parse, validate.
export async function validateBatchFile(file: File, ctx: ValidationContext): Promise<ValidationResult> {
  const text = await file.text();
  const parsed = parseJsonl(text);
  return validateBatchLines(parsed, ctx);
}

export function hasAnyOverride(overrides: BatchOverrides): boolean {
  return (
    overrides.maxTokens !== undefined ||
    overrides.temperature !== undefined ||
    overrides.topP !== undefined ||
    overrides.n !== undefined
  );
}

// Apply batch-level overrides to each parsed record.
// Semantics: if an override field is set (!== undefined), it replaces the
// per-line body[field]. If unset, per-line value is kept untouched.
// Override fields apply only to completion-style endpoints; lines targeting
// other endpoints (embeddings/rerank) are silently skipped and recorded.
export function applyBatchOverrides(
  records: ParsedLine[],
  overrides: BatchOverrides,
): { records: ParsedLine[]; diff: MutationDiff } {
  const diff: MutationDiff = {
    totalLines: records.length,
    changedLines: 0,
    fieldsChanged: {},
    skipped: [],
    samples: [],
  };

  if (!hasAnyOverride(overrides)) {
    return { records, diff };
  }

  const overrideEntries: Array<[keyof BatchOverrides, string, number]> = [];
  if (overrides.maxTokens !== undefined) overrideEntries.push(['maxTokens', OVERRIDE_FIELD_MAP.maxTokens, overrides.maxTokens]);
  if (overrides.temperature !== undefined) overrideEntries.push(['temperature', OVERRIDE_FIELD_MAP.temperature, overrides.temperature]);
  if (overrides.topP !== undefined) overrideEntries.push(['topP', OVERRIDE_FIELD_MAP.topP, overrides.topP]);
  if (overrides.n !== undefined) overrideEntries.push(['n', OVERRIDE_FIELD_MAP.n, overrides.n]);

  const out: ParsedLine[] = [];
  for (const { lineNumber, record } of records) {
    const url = typeof record.url === 'string' ? record.url : '';
    const isCompletion = COMPLETION_ENDPOINTS.has(url);
    const body: Record<string, unknown> = { ...(record.body ?? {}) };
    let lineChanged = false;

    for (const [, snake, value] of overrideEntries) {
      if (!isCompletion) {
        diff.skipped.push({
          lineNumber,
          field: snake,
          reason: `endpoint "${url || '<missing>'}" does not accept ${snake}`,
        });
        continue;
      }
      const before = body[snake];
      if (before === value) continue;
      body[snake] = value;
      diff.fieldsChanged[snake] = (diff.fieldsChanged[snake] ?? 0) + 1;
      if (diff.samples.length < MAX_DIFF_SAMPLES) {
        diff.samples.push({ lineNumber, field: snake, from: before, to: value });
      }
      lineChanged = true;
    }

    if (lineChanged) diff.changedLines++;
    out.push({ lineNumber, record: { ...record, body } });
  }

  return { records: out, diff };
}

export function serializeJsonl(records: ParsedLine[]): string {
  return records.map((r) => JSON.stringify(r.record)).join('\n') + '\n';
}

export function generateJobDisplayName(modelName: string): string {
  const now = new Date();
  const ts = now.toISOString().replace(/[-:T]/g, '').slice(0, 14);
  const short = modelName.replace(/[^a-zA-Z0-9._-]/g, '-').toLowerCase();
  return `${short}-batch-${ts}`;
}

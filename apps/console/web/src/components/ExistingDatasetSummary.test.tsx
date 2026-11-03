import { renderToStaticMarkup } from 'react-dom/server';
import { describe, expect, it } from 'vitest';
import { DatasetSummaryDetails } from './ExistingDatasetSummary';

describe('existing dataset details', () => {
  it('shows exact counts, model names and endpoints without a mismatch warning', () => {
    const html = renderToStaticMarkup(<DatasetSummaryDetails
      summary={{ requestCount: 12, invalidLines: 0, models: ['model-a'], endpoints: ['/v1/embeddings'], truncated: false }}
      expectedModel="model-a"
      supportedEndpoints={['/v1/embeddings']}
    />);
    expect(html).toContain('12');
    expect(html).toContain('model-a');
    expect(html).toContain('/v1/embeddings');
    expect(html).not.toContain('role="alert"');
    expect(html).not.toContain('At least');
  });

  it('labels partial counts and warns about invalid rows and incompatible datasets', () => {
    const html = renderToStaticMarkup(<DatasetSummaryDetails
      summary={{ requestCount: 3, invalidLines: 1, models: ['model-b'], endpoints: ['/v1/completions'], truncated: true }}
      expectedModel="model-a"
      supportedEndpoints={['/v1/embeddings']}
    />);
    expect(html).toContain('At least 3');
    expect(html).toContain('Preview is incomplete');
    expect(html).toContain('1 invalid');
    expect(html).toContain('does not match');
    expect(html).toContain('not supported');
  });

  it('warns about the mismatches that batch validation rejects at submit', () => {
    const mixed = renderToStaticMarkup(<DatasetSummaryDetails
      summary={{ requestCount: 4, invalidLines: 0, models: ['model-a', 'model-b'], endpoints: ['/v1/embeddings', '/v1/completions'], truncated: false }}
      expectedModel=""
      supportedEndpoints={['/v1/embeddings', '/v1/completions']}
    />);
    // expectedModel is empty and both endpoints are supported, so the two
    // existing warnings stay silent; these come only from the new checks.
    expect(mixed).toContain('more than one model');
    expect(mixed).toContain('more than one endpoint');
    expect(mixed).not.toContain('does not match');
    expect(mixed).not.toContain('not supported');
  });

  it('warns when the dataset is over the per-batch request limit', () => {
    const html = renderToStaticMarkup(<DatasetSummaryDetails
      summary={{ requestCount: 50001, invalidLines: 0, models: ['model-a'], endpoints: ['/v1/embeddings'], truncated: true }}
      expectedModel="model-a"
      supportedEndpoints={['/v1/embeddings']}
    />);
    expect(html).toContain('50,001');
    expect(html).toContain('per-batch limit is 50,000');
  });

  it('does not represent empty model and endpoint sets as a valid batch', () => {
    const html = renderToStaticMarkup(<DatasetSummaryDetails
      summary={{ requestCount: 0, invalidLines: 0, models: [], endpoints: [], truncated: false }}
      expectedModel="model-a"
      supportedEndpoints={['/v1/embeddings']}
    />);
    expect(html).toContain('No requests found');
  });
});

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

  it('does not represent empty model and endpoint sets as a valid batch', () => {
    const html = renderToStaticMarkup(<DatasetSummaryDetails
      summary={{ requestCount: 0, invalidLines: 0, models: [], endpoints: [], truncated: false }}
      expectedModel="model-a"
      supportedEndpoints={['/v1/embeddings']}
    />);
    expect(html).toContain('No requests found');
  });
});

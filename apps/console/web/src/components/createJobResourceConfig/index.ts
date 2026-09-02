export {
  getCreateJobResourcePlugin,
  registerCreateJobResourcePlugin,
  type CreateJobResourceConfig,
  type CreateJobResourceFieldsProps,
  type CreateJobResourcePlugin,
} from './registry';

import {
  createDemoResourcePlugin,
  DemoRenderer,
} from './DemoRenderer';
import {
  registerCreateJobResourcePlugin,
} from './registry';

const DemoResourceFields = DemoRenderer;

registerCreateJobResourcePlugin('demo', createDemoResourcePlugin(DemoResourceFields));

// Downstream plugins are registered through side-effect imports here.
import './extension';

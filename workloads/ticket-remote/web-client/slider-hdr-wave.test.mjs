import test from 'node:test';
import assert from 'node:assert/strict';
import { SliderHDRWave } from './slider-hdr-wave.mjs';

globalThis.requestAnimationFrame = (callback) => queueMicrotask(callback);
globalThis.GPUBufferUsage = { UNIFORM: 1, COPY_DST: 2 };
const settle = () => new Promise((resolve) => setImmediate(resolve));
function fixture(pipelinePromise) {
  const counts = { submissions: 0, destroyed: 0, deviceDestroyed: 0 };
  let config;
  const pipeline = { getBindGroupLayout: () => ({}) };
  const device = {
    queue: { writeBuffer() {}, submit() { counts.submissions++; } },
    createShaderModule: () => ({}), createRenderPipelineAsync: () => pipelinePromise || Promise.resolve(pipeline),
    createBuffer: () => ({ destroy() { counts.destroyed++; } }), createBindGroup: () => ({}),
    createCommandEncoder: () => ({ beginRenderPass: () => ({ setPipeline() {}, setBindGroup() {}, draw() {}, end() {} }), finish() {} }),
    destroy() { counts.deviceDestroyed++; }
  };
  const context = { configure(value) { config = value; }, getConfiguration: () => config,
    unconfigure() {}, getCurrentTexture: () => ({ createView: () => ({}) }) };
  const overlay = { dataset: {} }, canvas = { getContext: () => context };
  const renderer = { device, encodeOutput: false, context: { getConfiguration: () => ({ colorSpace: 'srgb-linear' }) } };
  return { wave: new SliderHDRWave(overlay, canvas), overlay, renderer, counts, pipeline };
}
const bounds = { width: 200, height: 40 };

test('reuse static HDR output; redraw only for geometry or boost; never destroy stream device', async () => {
  const f = fixture();
  f.wave.update(f.renderer, 4, bounds); await settle();
  assert.equal(f.overlay.dataset.hdrWave, 'true');
  for (let i = 0; i < 20; i++) f.wave.update(f.renderer, 4, bounds);
  assert.equal(f.counts.submissions, 1);
  f.wave.update(f.renderer, 6, bounds);
  f.wave.update(f.renderer, 6, { ...bounds, width: 250 });
  assert.equal(f.counts.submissions, 3);
  f.wave.update(f.renderer, 6, null);
  assert.equal(f.overlay.dataset.hdrWave, undefined);
  f.wave.update(f.renderer, 6, { ...bounds, width: 250 }); await settle();
  assert.equal(f.counts.submissions, 4);
  f.wave.dispose();
  assert.equal(f.counts.destroyed, 1);
  assert.equal(f.counts.deviceDestroyed, 0);
});

test('late initialization cannot restore HDR after disabling or replacing the device', async () => {
  let resolve;
  const f = fixture(new Promise((done) => { resolve = done; }));
  f.wave.update(f.renderer, 4, bounds);
  f.wave.update(null, 4, null);
  resolve(f.pipeline); await settle();
  assert.equal(f.overlay.dataset.hdrWave, undefined);
  assert.equal(f.counts.submissions, 0);
});

test('wave failure leaves SDR available and does not continually retry on the same device', async () => {
  const f = fixture(); let attempts = 0;
  f.renderer.device.createRenderPipelineAsync = async () => { attempts++; throw new Error('unsupported'); };
  f.wave.update(f.renderer, 4, bounds); await settle();
  f.wave.update(f.renderer, 4, bounds); await settle();
  assert.equal(f.overlay.dataset.hdrWave, undefined);
  assert.equal(attempts, 1);
  assert.equal(f.counts.deviceDestroyed, 0);
});

import test from 'node:test';
import assert from 'node:assert/strict';
import { ClientHDRRenderer } from './client-hdr-renderer.mjs';

const deferred = () => {
  let resolve;
  const promise = new Promise((done) => { resolve = done; });
  return { promise, resolve };
};

test('validated GPU work completes and releases its deadline', async () => {
  const queue = deferred(), validation = deferred();
  const { renderer, failures } = rendererWith(queue, validation);
  const work = renderer.submitAndWait(() => {}, 'test_submit');
  queue.resolve();
  validation.resolve(null);
  await work;
  assert.equal(renderer.completionWaits.size, 0);
  assert.deepEqual(failures, []);
  renderer.dispose();
});

function rendererWith(queue, validation) {
  const failures = [];
  const renderer = new ClientHDRRenderer({ onFailure: (reason) => failures.push(reason) });
  let pops = 0;
  renderer.device = {
    pushErrorScope() {},
    popErrorScope() { pops++; return validation.promise; },
    queue: { onSubmittedWorkDone: () => queue.promise },
    destroy() {}
  };
  return { renderer, failures, pops: () => pops };
}

test('GPU submission waits for queue completion and validation, and rejects invalid work', async () => {
  const queue = deferred(), validation = deferred();
  const { renderer, failures, pops } = rendererWith(queue, validation);
  let completed = false;
  const work = renderer.submitAndWait(() => {}, 'test_submit').then(() => { completed = true; });
  await Promise.resolve();
  assert.equal(completed, false);
  queue.resolve();
  await new Promise(setImmediate);
  assert.equal(pops(), 1);
  assert.equal(completed, false);
  validation.resolve({ message: 'invalid texture' });
  await assert.rejects(work, /invalid_texture/);
  assert.equal(completed, false);
  assert.equal(pops(), 1);
  assert.equal(failures.length, 1);
  renderer.dispose();
});

test('dispose and deadline cancel a pending submission without accepting its late completion', async () => {
  for (const terminate of ['dispose', 'deadline']) {
    const queue = deferred(), validation = deferred();
    const { renderer } = rendererWith(queue, validation);
    let deadline;
    renderer.setTimer = (callback) => { deadline = callback; return 1; };
    renderer.clearTimer = () => {};
    let accepted = false;
    const work = renderer.submitAndWait(() => {}, 'test_submit').then(() => { accepted = true; });
    const rejected = assert.rejects(work, /renderer_disposed|test_submit_timeout/);
    if (terminate === 'dispose') renderer.dispose();
    else deadline();
    await rejected;
    queue.resolve();
    validation.resolve(null);
    await new Promise(setImmediate);
    assert.equal(accepted, false);
    assert.equal(renderer.completionWaits.size, 0);
    renderer.dispose();
  }
});

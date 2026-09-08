import test from 'node:test';
import assert from 'node:assert/strict';
import { exactResultMatches } from './exact-result.mjs';

test('only the current requested result picture can be presented', () => {
  const request = { status: 'succeeded', captureRequired: true,
    resultFrameEpoch: '7', resultMinFrameSequence: '42', resultMarkerRevision: '7:42' };
  assert.equal(exactResultMatches(request, 7, 42), true);
  for (const [epoch, sequence] of [[6, 42], [8, 42], [7, 41], [7, 43], [0, 0]]) {
    assert.equal(exactResultMatches(request, epoch, sequence), false);
  }
  assert.equal(exactResultMatches({ ...request, resultMarkerRevision: '7:43' }, 7, 42), false);
  assert.equal(exactResultMatches({ ...request, captureRequired: false }, 7, 42), false);
  assert.equal(exactResultMatches({ ...request, status: 'closed' }, 7, 42), false);
  const refreshed = { ...request, resultMinFrameSequence: '44', resultMarkerRevision: '7:44' };
  assert.equal(exactResultMatches(refreshed, 7, 42), false);
  assert.equal(exactResultMatches(refreshed, 7, 44), true);
});

import test from 'node:test';
import assert from 'node:assert/strict';
import { StartupMeasurement } from './startup-measurement.mjs';

test('records navigation to post-paint and ten distinct pictures, once', () => {
  const events = [];
  const run = new StartupMeasurement(value => events.push(value));
  run.connect(); run.opening('warm');
  run.presented({epoch:1,sequence:1},450.3);
  for (let i=0; i<20; i++) run.presented({epoch:1,sequence:1},500+i);
  assert.equal(run.snapshot().presentedFrames,1);
  run.connect(); run.opening('recovery');
  for (let sequence=2; sequence<=10; sequence++) run.presented({epoch:1,sequence},450+(sequence-1)*1000);
  run.flush(); run.presented({epoch:1,sequence:11},11000);
  assert.deepEqual(events,[{firstPresentedMillis:450,tenPresentedMillis:9450,presentedFrames:10,reconnects:1,openingClass:'warm'}]);
});
test('leaving early preserves one bounded partial opening measurement', () => {
  const events=[];
  const run=new StartupMeasurement(value=>events.push(value));
  run.flush(); assert.equal(events.length,0);
  run.presented({epoch:3,sequence:1},900);
  run.flush(); run.flush();
  assert.equal(events.length,1);
  assert.equal(events[0].tenPresentedMillis,null);
});
